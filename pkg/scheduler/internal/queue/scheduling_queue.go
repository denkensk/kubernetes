/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// This file contains structures that implement scheduling queue types.
// Scheduling queues hold pods waiting to be scheduled. This file has two types
// of scheduling queue: 1) a FIFO, which is mostly the same as cache.FIFO, 2) a
// priority queue which has two sub queues. One sub-queue holds pods that are
// being considered for scheduling. This is called activeQ. Another queue holds
// pods that are already tried and are determined to be unschedulable. The latter
// is called unschedulableQ.
// FIFO is here for flag-gating purposes and allows us to use the traditional
// scheduling queue when util.PodPriorityEnabled() returns false.

package queue

import (
	"container/heap"
	"fmt"
	"reflect"
	"sync"

	"github.com/golang/glog"

	"container/list"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/scheduler/algorithm/predicates"
	priorityutil "k8s.io/kubernetes/pkg/scheduler/algorithm/priorities/util"
	"k8s.io/kubernetes/pkg/scheduler/core/equivalence"
	"k8s.io/kubernetes/pkg/scheduler/util"
)

var (
	queueClosed = "scheduling queue is closed"
)

// SchedulingQueue is an interface for a queue to store pods waiting to be scheduled.
// The interface follows a pattern similar to cache.FIFO and cache.Heap and
// makes it easy to use those data structures as a SchedulingQueue.
type SchedulingQueue interface {
	Add(pod *v1.Pod) error
	AddIfNotPresent(pod *v1.Pod) error
	AddUnschedulableIfNotPresent(pod *v1.Pod) error
	// Pop removes the head of the queue and returns it. It blocks if the
	// queue is empty and waits until a new item is added to the queue.
	Pop() (*v1.Pod, error)
	Update(oldPod, newPod *v1.Pod) error
	Delete(pod *v1.Pod) error
	MoveAllToActiveQueue()
	AssignedPodAdded(pod *v1.Pod)
	AssignedPodUpdated(pod *v1.Pod)
	WaitingPodsForNode(nodeName string) []*v1.Pod
	WaitingPods() []*v1.Pod
	// Close closes the SchedulingQueue so that the goroutine which is
	// waiting to pop items can exit gracefully.
	Close()
}

// NewSchedulingQueue initializes a new scheduling queue. If pod priority is
// enabled a priority queue is returned. If it is disabled, a FIFO is returned.
func NewSchedulingQueue() SchedulingQueue {
	if util.PodPriorityEnabled() {
		return NewPriorityQueue()
	}
	return NewFIFO()
}

// FIFO is basically a simple wrapper around cache.FIFO to make it compatible
// with the SchedulingQueue interface.
type FIFO struct {
	*cache.FIFO
}

var _ = SchedulingQueue(&FIFO{}) // Making sure that FIFO implements SchedulingQueue.

// Add adds a pod to the FIFO.
func (f *FIFO) Add(pod *v1.Pod) error {
	return f.FIFO.Add(pod)
}

// AddIfNotPresent adds a pod to the FIFO if it is absent in the FIFO.
func (f *FIFO) AddIfNotPresent(pod *v1.Pod) error {
	return f.FIFO.AddIfNotPresent(pod)
}

// AddUnschedulableIfNotPresent adds an unschedulable pod back to the queue. In
// FIFO it is added to the end of the queue.
func (f *FIFO) AddUnschedulableIfNotPresent(pod *v1.Pod) error {
	return f.FIFO.AddIfNotPresent(pod)
}

// Update updates a pod in the FIFO.
func (f *FIFO) Update(oldPod, newPod *v1.Pod) error {
	return f.FIFO.Update(newPod)
}

// Delete deletes a pod in the FIFO.
func (f *FIFO) Delete(pod *v1.Pod) error {
	return f.FIFO.Delete(pod)
}

// Pop removes the head of FIFO and returns it.
// This is just a copy/paste of cache.Pop(queue Queue) from fifo.go that scheduler
// has always been using. There is a comment in that file saying that this method
// shouldn't be used in production code, but scheduler has always been using it.
// This function does minimal error checking.
func (f *FIFO) Pop() (*v1.Pod, error) {
	result, err := f.FIFO.Pop(func(obj interface{}) error { return nil })
	if err == cache.FIFOClosedError {
		return nil, fmt.Errorf(queueClosed)
	}
	return result.(*v1.Pod), err
}

// WaitingPods returns all the waiting pods in the queue.
func (f *FIFO) WaitingPods() []*v1.Pod {
	result := []*v1.Pod{}
	for _, pod := range f.FIFO.List() {
		result = append(result, pod.(*v1.Pod))
	}
	return result
}

// FIFO does not need to react to events, as all pods are always in the active
// scheduling queue anyway.

// AssignedPodAdded does nothing here.
func (f *FIFO) AssignedPodAdded(pod *v1.Pod) {}

// AssignedPodUpdated does nothing here.
func (f *FIFO) AssignedPodUpdated(pod *v1.Pod) {}

// MoveAllToActiveQueue does nothing in FIFO as all pods are always in the active queue.
func (f *FIFO) MoveAllToActiveQueue() {}

// WaitingPodsForNode returns pods that are nominated to run on the given node,
// but FIFO does not support it.
func (f *FIFO) WaitingPodsForNode(nodeName string) []*v1.Pod {
	return nil
}

// Close closes the FIFO queue.
func (f *FIFO) Close() {
	f.FIFO.Close()
}

// NewFIFO creates a FIFO object.
func NewFIFO() *FIFO {
	return &FIFO{FIFO: cache.NewFIFO(cache.MetaNamespaceKeyFunc)}
}

// NominatedNodeName returns nominated node name of a Pod.
func NominatedNodeName(pod *v1.Pod) string {
	return pod.Status.NominatedNodeName
}

// PriorityQueue implements a scheduling queue. It is an alternative to FIFO.
// The head of PriorityQueue is the highest priority pending pod. This structure
// has two sub queues. One sub-queue holds pods that are being considered for
// scheduling. This is called activeQ and is a Heap. Another queue holds
// pods that are already tried and are determined to be unschedulable. The latter
// is called unschedulableQ.
type PriorityQueue struct {
	lock sync.RWMutex
	cond sync.Cond

	// activeQ is heap structure that scheduler actively looks at to find pods to
	// schedule. Head of heap is the highest priority pod.
	activeQ *Heap
	// unschedulableQ holds pods that have been tried and determined unschedulable.
	unschedulableQ *UnschedulableequivalenceClassesMap
	// nominatedPods is a map keyed by a node name and the value is a list of
	// pods which are nominated to run on the node. These are pods which can be in
	// the activeQ or unschedulableQ.
	nominatedPods map[string][]*v1.Pod
	// receivedMoveRequest is set to true whenever we receive a request to move a
	// pod from the unschedulableQ to the activeQ, and is set to false, when we pop
	// a pod from the activeQ. It indicates if we received a move request when a
	// pod was in flight (we were trying to schedule it). In such a case, we put
	// the pod back into the activeQ if it is determined unschedulable.
	receivedMoveRequest bool

	// closed indicates that the queue is closed.
	// It is mainly used to let Pop() exit its control loop while waiting for an item.
	closed bool

	equivalenceCache *equivalence.EquivalenceCache
}

// Making sure that PriorityQueue implements SchedulingQueue.
var _ = SchedulingQueue(&PriorityQueue{})

// NewPriorityQueue creates a PriorityQueue object.
func NewPriorityQueue() *PriorityQueue {
	pq := &PriorityQueue{
		activeQ:          newHeap(equivalence.MetaNamespaceKeyFunc, equivalence.HigherPriorityEquivalenceClass),
		unschedulableQ:   newUnschedulablePodsMap(),
		nominatedPods:    map[string][]*v1.Pod{},
		equivalenceCache: equivalence.NewEquivalenceCache(),
	}
	pq.cond.L = &pq.lock
	return pq
}

// addNominatedPodIfNeeded adds a pod to nominatedPods if it has a NominatedNodeName and it does not
// already exist in the map. Adding an existing pod is not going to update the pod.
func (p *PriorityQueue) addNominatedPodIfNeeded(pod *v1.Pod) {
	nnn := NominatedNodeName(pod)
	if len(nnn) > 0 {
		for _, np := range p.nominatedPods[nnn] {
			if np.UID == pod.UID {
				glog.Errorf("Pod %v/%v already exists in the nominated map!", pod.Namespace, pod.Name)
				return
			}
		}
		p.nominatedPods[nnn] = append(p.nominatedPods[nnn], pod)
	}
}

// deleteNominatedPodIfExists deletes a pod from the nominatedPods.
func (p *PriorityQueue) deleteNominatedPodIfExists(pod *v1.Pod) {
	nnn := NominatedNodeName(pod)
	if len(nnn) > 0 {
		for i, np := range p.nominatedPods[nnn] {
			if np.UID == pod.UID {
				p.nominatedPods[nnn] = append(p.nominatedPods[nnn][:i], p.nominatedPods[nnn][i+1:]...)
				if len(p.nominatedPods[nnn]) == 0 {
					delete(p.nominatedPods, nnn)
				}
				break
			}
		}
	}
}

// updateNominatedPod updates a pod in the nominatedPods.
func (p *PriorityQueue) updateNominatedPod(oldPod, newPod *v1.Pod) {
	// Even if the nominated node name of the Pod is not changed, we must delete and add it again
	// to ensure that its pointer is updated.
	p.deleteNominatedPodIfExists(oldPod)
	p.addNominatedPodIfNeeded(newPod)
}

// Add adds a pod to the active queue. It should be called only when a new pod
// is added so there is no chance the pod is already in either queue.
func (p *PriorityQueue) Add(pod *v1.Pod) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	hash := equivalence.GetEquivHash(pod)
	glog.V(4).Infof("ADD: hash: s%", hash)
	glog.Errorf("ADD: hash: s%", hash)
	var equivalenceClass *equivalence.Class
	if _, ok := p.equivalenceCache.Cache[hash]; ok {
		equivalenceClass = p.equivalenceCache.Cache[hash]
		equivalenceClass.Mu.Lock()
		equivalenceClass.PodList.PushBack(pod)
		glog.V(4).Infof("ADD: PodList-Len-1: d%", equivalenceClass.PodList.Len())
		glog.V(4).Info(equivalenceClass.PodList)
		glog.Errorf("ADD: PodList-Len-1: d%", equivalenceClass.PodList.Len())
		equivalenceClass.Mu.Unlock()

	} else {
		equivalenceClass = equivalence.NewClass(pod)
		equivalenceClass.PodList.PushBack(pod)
		p.equivalenceCache.Cache[hash] = equivalenceClass
		glog.V(4).Infof("ADD: PodList-Len-2: d%", equivalenceClass.PodList.Len())
		glog.Errorf("ADD: PodList-Len-2: d%", equivalenceClass.PodList.Len())
	}

	// equivalenceClass is new to activeQ, add it.
	err := p.activeQ.Add(equivalenceClass)
	if err != nil {
		glog.Errorf("Error adding pod %v/%v to the scheduling queue: %v", pod.Namespace, pod.Name, err)
	} else {
		if p.unschedulableQ.get(equivalenceClass) != nil {
			glog.Errorf("Error: pod %v/%v is already in the unschedulable queue.", pod.Namespace, pod.Name)
			p.deleteNominatedPodIfExists(pod)
			p.unschedulableQ.delete(equivalenceClass)
		}
		p.addNominatedPodIfNeeded(pod)
		p.cond.Broadcast()
	}
	glog.Errorf("ADD: p.activeQ.data.queue %d", len(p.activeQ.data.queue))
	return err
}

// AddIfNotPresent adds a pod to the active queue if it is not present in any of
// the two queues. If it is present in any, it doesn't do any thing.
func (p *PriorityQueue) AddIfNotPresent(pod *v1.Pod) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	equivalenceClass := equivalence.NewClass(pod)
	var next *list.Element
	for e := equivalenceClass.PodList.Front(); e != nil; e = next {
		if e.Value.(*v1.Pod).Name == pod.Name {
			return nil
		}
		next = e.Next()
	}
	equivalenceClass.PodList.PushBack(pod)

	if p.unschedulableQ.get(equivalenceClass) != nil {
		return nil
	}

	p.addNominatedPodIfNeeded(pod)
	p.cond.Broadcast()

	_, okActiveQ, _ := p.activeQ.Get(equivalenceClass)

	if okActiveQ {
		return nil
	}

	p.equivalenceCache.Cache[equivalence.GetEquivHash(pod)] = equivalenceClass
	err := p.activeQ.Add(equivalenceClass)
	if err != nil {
		glog.Errorf("Error adding pod %v/%v to the scheduling queue: %v", pod.Namespace, pod.Name, err)
	}

	return err
}

func isPodUnschedulable(pod *v1.Pod) bool {
	_, cond := podutil.GetPodCondition(&pod.Status, v1.PodScheduled)
	return cond != nil && cond.Status == v1.ConditionFalse && cond.Reason == v1.PodReasonUnschedulable
}

// AddUnschedulableIfNotPresent does nothing if the pod is present in either
// queue. Otherwise it adds the pod to the unschedulable queue if
// p.receivedMoveRequest is false, and to the activeQ if p.receivedMoveRequest is true.
func (p *PriorityQueue) AddUnschedulableIfNotPresent(pod *v1.Pod) error {
	p.lock.Lock()
	defer p.lock.Unlock()
	equivalenceClass := equivalence.NewClass(pod)
	if p.equivalenceCache.Cache[equivalenceClass.Hash] == nil {
		p.equivalenceCache.Cache[equivalenceClass.Hash] = equivalenceClass
	}

	var next *list.Element
	var exit bool
	for e := equivalenceClass.PodList.Front(); e != nil; e = next {
		if e.Value.(*v1.Pod).Name == pod.Name {
			exit = true
		}
		next = e.Next()
	}
	if !exit {
		equivalenceClass.PodList.PushBack(pod)
		p.addNominatedPodIfNeeded(pod)
	}

	if p.unschedulableQ.get(equivalenceClass) != nil {
		glog.Info("pod is already present in unschedulableQ")
		return fmt.Errorf("pod is already present in unschedulableQ")
	}

	//equivalenceClass.Mu.RLock()
	//defer equivalenceClass.Mu.RUnlock()
	if _, exists, _ := p.activeQ.Get(equivalenceClass); exists {
		glog.Info("pod is already present in the activeQ")
		return fmt.Errorf("pod is already present in the activeQ")
	}

	if !p.receivedMoveRequest && isPodUnschedulable(pod) {
		p.unschedulableQ.addOrUpdate(equivalenceClass)
		return nil
	}
	err := p.activeQ.Add(equivalenceClass)
	//glog.V(4).Infof("AddUnschedulableIfNotPresent equivalenceClass.PodList.Len() d%", equivalenceClass.PodList.Len())
	if err == nil {
		p.cond.Broadcast()
	}
	return err
}

// Pop removes the head of the active queue and returns it. It blocks if the
// activeQ is empty and waits until a new item is added to the queue. It also
// clears receivedMoveRequest to mark the beginning of a new scheduling cycle.
func (p *PriorityQueue) Pop() (*v1.Pod, error) {
	p.lock.Lock()
	defer p.lock.Unlock()
	for {
		glog.Errorf("POP: p.activeQ.data.queue %d", len(p.activeQ.data.queue))
		for len(p.activeQ.data.queue) == 0 {
			// When the queue is empty, invocation of Pop() is blocked until new item is enqueued.
			// When Close() is called, the p.closed is set and the condition is broadcast,
			// which causes this loop to continue and return from the Pop().
			if p.closed {
				return nil, fmt.Errorf(queueClosed)
			}
			p.cond.Wait()
		}

		obj, err := p.activeQ.Pop()
		equivalenceClass := obj.(*equivalence.Class)

		//p.deleteNominatedPodIfExists(pod)

		p.receivedMoveRequest = false

		glog.Errorf("equivalenceClass.PodList.Len() %d", equivalenceClass.PodList.Len())

		// check the podList len
		if equivalenceClass.PodList.Len() > 0 {
			l := equivalenceClass.PodList
			var next *list.Element
			for e := l.Front(); e != nil; e = next {
				pod := e.Value.(*v1.Pod)
				p.deleteNominatedPodIfExists(pod)
				next = e.Next()
			}
			return equivalenceClass.PodList.Front().Value.(*v1.Pod), err
		}
	}

	/*
		obj, err := p.activeQ.Pop()
		if err != nil {
			return nil, err
		}
		pod := obj.(*v1.Pod)
		p.deleteNominatedPodIfExists(pod)
		p.receivedMoveRequest = false
		return pod, err
	*/
}

// isPodUpdated checks if the pod is updated in a way that it may have become
// schedulable. It drops status of the pod and compares it with old version.
func isPodUpdated(oldPod, newPod *v1.Pod) bool {
	strip := func(pod *v1.Pod) *v1.Pod {
		p := pod.DeepCopy()
		p.ResourceVersion = ""
		p.Generation = 0
		p.Status = v1.PodStatus{}
		return p
	}
	return !reflect.DeepEqual(strip(oldPod), strip(newPod))
}

// Update updates a pod in the active queue if present. Otherwise, it removes
// the item from the unschedulable queue and adds the updated one to the active
// queue.
func (p *PriorityQueue) Update(oldPod, newPod *v1.Pod) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	if oldPod != nil {
		hash := equivalence.GetEquivHash(oldPod)
		equivalenceClass := p.equivalenceCache.Cache[hash]
		if equivalenceClass != nil {
			if _, exists, _ := p.activeQ.Get(equivalenceClass); exists {
				p.updateNominatedPod(oldPod, newPod)
				var next *list.Element
				for e := equivalenceClass.PodList.Front(); e != nil; e = next {
					if e.Value.(*v1.Pod).Name == oldPod.Name {
						e.Value = newPod
						return nil
					}
					next = e.Next()
				}
			}

			// If the pod is in the unschedulable queue, updating it may make it schedulable.
			if usPod := p.unschedulableQ.get(equivalenceClass); usPod != nil {
				p.updateNominatedPod(oldPod, newPod)
				if isPodUpdated(oldPod, newPod) {
					var next *list.Element
					for e := equivalenceClass.PodList.Front(); e != nil; e = next {
						if e.Value.(*v1.Pod).Name == oldPod.Name {
							e.Value = newPod
							return nil
						}
						next = e.Next()
					}
				}
			}
		}
	}

	if newPod != nil {
		// If pod is not in any of the two queue, we put it in the active queue.
		equivalenceClass := equivalence.NewClass(newPod)
		equivalenceClass.PodList.PushBack(newPod)
		p.equivalenceCache.Cache[equivalenceClass.Hash] = equivalenceClass
		err := p.activeQ.Add(equivalenceClass)
		if err == nil {
			p.addNominatedPodIfNeeded(newPod)
			p.cond.Broadcast()
		}
		return err
	}
	return nil
}

// Delete deletes the item from either of the two queues. It assumes the pod is
// only in one queue.
func (p *PriorityQueue) Delete(pod *v1.Pod) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	hash := equivalence.GetEquivHash(pod)
	if equivalenceClass, ok := p.equivalenceCache.Cache[hash]; ok {
		var next *list.Element
		for e := equivalenceClass.PodList.Front(); e != nil; e = next {
			if e.Value.(*v1.Pod) == pod {
				equivalenceClass.PodList.Remove(e)
				break
			}
			next = e.Next()
		}

		p.deleteNominatedPodIfExists(pod)
		if equivalenceClass.PodList.Len() == 0 {
			err := p.activeQ.Delete(equivalenceClass)
			if err != nil { // The item was probably not found in the activeQ.
				p.unschedulableQ.delete(equivalenceClass)
			}
		}

		p.deleteNominatedPodIfExists(pod)
		err := p.activeQ.Delete(equivalenceClass)
		if err != nil { // The item was probably not found in the activeQ.
			p.unschedulableQ.delete(equivalenceClass)
		}
		return nil
	}
	return nil
}

// AssignedPodAdded is called when a bound pod is added. Creation of this pod
// may make pending pods with matching affinity terms schedulable.
func (p *PriorityQueue) AssignedPodAdded(pod *v1.Pod) {
	p.movePodsToActiveQueue(p.getUnschedulablePodsWithMatchingAffinityTerm(pod))
}

// AssignedPodUpdated is called when a bound pod is updated. Change of labels
// may make pending pods with matching affinity terms schedulable.
func (p *PriorityQueue) AssignedPodUpdated(pod *v1.Pod) {
	p.movePodsToActiveQueue(p.getUnschedulablePodsWithMatchingAffinityTerm(pod))
}

// MoveAllToActiveQueue moves all pods from unschedulableQ to activeQ. This
// function adds all pods and then signals the condition variable to ensure that
// if Pop() is waiting for an item, it receives it after all the pods are in the
// queue and the head is the highest priority pod.
// TODO(bsalamat): We should add a back-off mechanism here so that a high priority
// pod which is unschedulable does not go to the head of the queue frequently. For
// example in a cluster where a lot of pods being deleted, such a high priority
// pod can deprive other pods from getting scheduled.
func (p *PriorityQueue) MoveAllToActiveQueue() {
	p.lock.Lock()
	defer p.lock.Unlock()
	for _, equivalenceClass := range p.unschedulableQ.equivalenceClasses {
		if err := p.activeQ.Add(equivalenceClass); err != nil {
			glog.Errorf("Error adding pod to the scheduling queue: %v", equivalenceClass)
		}
	}
	p.unschedulableQ.clear()
	p.receivedMoveRequest = true
	p.cond.Broadcast()
}

func (p *PriorityQueue) movePodsToActiveQueue(pods []*v1.Pod) {
	p.lock.Lock()
	defer p.lock.Unlock()
	for _, pod := range pods {
		hash := equivalence.GetEquivHash(pod)
		var equivalenceClass *equivalence.Class
		if _, ok := p.equivalenceCache.Cache[hash]; ok {
			equivalenceClass = p.equivalenceCache.Cache[hash]
			equivalenceClass.PodList.PushBack(pod)
		} else {
			equivalenceClass = equivalence.NewClass(pod)
			equivalenceClass.PodList.PushBack(pod)
			p.equivalenceCache.Cache[hash] = equivalenceClass
		}
		glog.V(4).Info("movePodsToActiveQueue.....")
		if err := p.activeQ.Add(equivalenceClass); err == nil {
			p.unschedulableQ.delete(equivalenceClass)
		} else {
			glog.Errorf("Error adding pod %v/%v to the scheduling queue: %v", pod.Namespace, pod.Name, err)
		}
	}
	p.receivedMoveRequest = true
	p.cond.Broadcast()
}

// getUnschedulablePodsWithMatchingAffinityTerm returns unschedulable pods which have
// any affinity term that matches "pod".
func (p *PriorityQueue) getUnschedulablePodsWithMatchingAffinityTerm(pod *v1.Pod) []*v1.Pod {
	p.lock.RLock()
	defer p.lock.RUnlock()
	var podsToMove []*v1.Pod

	glog.Errorf("len(p.unschedulableQ.equivalenceClasses) %d.", len(p.unschedulableQ.equivalenceClasses))
	var next *list.Element
	for _, equivalenceClass := range p.unschedulableQ.equivalenceClasses {
		glog.Errorf("equivalenceClass: %v.", equivalenceClass)
		for e := equivalenceClass.PodList.Front(); e != nil; e = next {
			affinity := e.Value.(*v1.Pod).Spec.Affinity
			glog.Errorf("pod: %v.", affinity)
			if affinity != nil && affinity.PodAffinity != nil {
				terms := predicates.GetPodAffinityTerms(affinity.PodAffinity)
				glog.Errorf("terms: %v.", terms)
				for _, term := range terms {
					namespaces := priorityutil.GetNamespacesFromPodAffinityTerm(e.Value.(*v1.Pod), &term)
					selector, err := metav1.LabelSelectorAsSelector(term.LabelSelector)
					if err != nil {
						glog.Errorf("Error getting label selectors for pod: %v.", e.Value.(*v1.Pod).Name)
					}
					if priorityutil.PodMatchesTermsNamespaceAndSelector(pod, namespaces, selector) {
						podsToMove = append(podsToMove, e.Value.(*v1.Pod))
						break
					}
				}
			}
			next = e.Next()
		}
	}
	glog.Errorf("podsToMove: %v.", podsToMove)
	return podsToMove
}

// WaitingPodsForNode returns pods that are nominated to run on the given node,
// but they are waiting for other pods to be removed from the node before they
// can be actually scheduled.
func (p *PriorityQueue) WaitingPodsForNode(nodeName string) []*v1.Pod {
	p.lock.RLock()
	defer p.lock.RUnlock()
	if list, ok := p.nominatedPods[nodeName]; ok {
		return list
	}
	return nil
}

// WaitingPods returns all the waiting pods in the queue.
func (p *PriorityQueue) WaitingPods() []*v1.Pod {
	p.lock.Lock()
	defer p.lock.Unlock()
	podMap := make(map[types.UID]*v1.Pod)
	result := []*v1.Pod{}
	var next *list.Element
	for _, equivalenceClass := range p.activeQ.List() {
		for e := equivalenceClass.(*equivalence.Class).PodList.Front(); e != nil; e = next {
			podMap[e.Value.(*v1.Pod).UID] = e.Value.(*v1.Pod)
			next = e.Next()
		}
	}

	for _, equivalenceClass := range p.unschedulableQ.equivalenceClasses {
		for e := equivalenceClass.PodList.Front(); e != nil; e = next {
			podMap[e.Value.(*v1.Pod).UID] = e.Value.(*v1.Pod)
			next = e.Next()
		}
	}

	for _, v := range podMap {
		result = append(result, v)
	}

	return result
}

// Close closes the priority queue.
func (p *PriorityQueue) Close() {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.closed = true
	p.cond.Broadcast()
}

// UnschedulablePodsMap holds pods that cannot be scheduled. This data structure
// is used to implement unschedulableQ.
type UnschedulableequivalenceClassesMap struct {
	// pods is a map key by a pod's full-name and the value is a pointer to the pod.
	//pods    map[string]*v1.Pod
	//keyFunc func(*v1.Pod) string
	equivalenceClasses map[uint64]*equivalence.Class
}

// Add adds a pod to the unschedulable pods.
func (u *UnschedulableequivalenceClassesMap) addOrUpdate(equivalenceClass *equivalence.Class) {
	u.equivalenceClasses[equivalenceClass.Hash] = equivalenceClass
}

// Delete deletes a pod from the unschedulable pods.
func (u *UnschedulableequivalenceClassesMap) delete(equivalenceClass *equivalence.Class) {
	delete(u.equivalenceClasses, equivalenceClass.Hash)
}

// Get returns the pod if a pod with the same key as the key of the given "pod"
// is found in the map. It returns nil otherwise.
func (u *UnschedulableequivalenceClassesMap) get(equivalenceClass *equivalence.Class) *equivalence.Class {
	equivalenceClassKey := equivalenceClass.Hash
	if p, exists := u.equivalenceClasses[equivalenceClassKey]; exists {
		return p
	}
	return nil
}

// Clear removes all the entries from the unschedulable maps.
func (u *UnschedulableequivalenceClassesMap) clear() {
	u.equivalenceClasses = make(map[uint64]*equivalence.Class)
}

// newUnschedulablePodsMap initializes a new object of UnschedulablePodsMap.
func newUnschedulablePodsMap() *UnschedulableequivalenceClassesMap {
	return &UnschedulableequivalenceClassesMap{
		equivalenceClasses: make(map[uint64]*equivalence.Class),
	}
}

// Below is the implementation of the a heap. The logic is pretty much the same
// as cache.heap, however, this heap does not perform synchronization. It leaves
// synchronization to the SchedulingQueue.

// LessFunc is a function type to compare two objects.
type LessFunc func(interface{}, interface{}) bool

// KeyFunc is a function type to get the key from an object.
type KeyFunc func(obj interface{}) (string, error)

type heapItem struct {
	obj   interface{} // The object which is stored in the heap.
	index int         // The index of the object's key in the Heap.queue.
}

type itemKeyValue struct {
	key string
	obj interface{}
}

// heapData is an internal struct that implements the standard heap interface
// and keeps the data stored in the heap.
type heapData struct {
	// items is a map from key of the objects to the objects and their index.
	// We depend on the property that items in the map are in the queue and vice versa.
	items map[string]*heapItem
	// queue implements a heap data structure and keeps the order of elements
	// according to the heap invariant. The queue keeps the keys of objects stored
	// in "items".
	queue []string

	// keyFunc is used to make the key used for queued item insertion and retrieval, and
	// should be deterministic.
	keyFunc KeyFunc
	// lessFunc is used to compare two objects in the heap.
	lessFunc LessFunc
}

var (
	_ = heap.Interface(&heapData{}) // heapData is a standard heap
)

// Less compares two objects and returns true if the first one should go
// in front of the second one in the heap.
func (h *heapData) Less(i, j int) bool {
	if i > len(h.queue) || j > len(h.queue) {
		return false
	}
	itemi, ok := h.items[h.queue[i]]
	if !ok {
		return false
	}
	itemj, ok := h.items[h.queue[j]]
	if !ok {
		return false
	}
	return h.lessFunc(itemi.obj, itemj.obj)
}

// Len returns the number of items in the Heap.
func (h *heapData) Len() int { return len(h.queue) }

// Swap implements swapping of two elements in the heap. This is a part of standard
// heap interface and should never be called directly.
func (h *heapData) Swap(i, j int) {
	h.queue[i], h.queue[j] = h.queue[j], h.queue[i]
	item := h.items[h.queue[i]]
	item.index = i
	item = h.items[h.queue[j]]
	item.index = j
}

// Push is supposed to be called by heap.Push only.
func (h *heapData) Push(kv interface{}) {
	keyValue := kv.(*itemKeyValue)
	n := len(h.queue)
	h.items[keyValue.key] = &heapItem{keyValue.obj, n}
	h.queue = append(h.queue, keyValue.key)
}

// Pop is supposed to be called by heap.Pop only.
func (h *heapData) Pop() interface{} {
	key := h.queue[len(h.queue)-1]
	h.queue = h.queue[0 : len(h.queue)-1]
	item, ok := h.items[key]
	if !ok {
		// This is an error
		return nil
	}
	delete(h.items, key)
	return item.obj
}

// Heap is a producer/consumer queue that implements a heap data structure.
// It can be used to implement priority queues and similar data structures.
type Heap struct {
	// data stores objects and has a queue that keeps their ordering according
	// to the heap invariant.
	data *heapData
}

// Add inserts an item, and puts it in the queue. The item is updated if it
// already exists.
func (h *Heap) Add(obj interface{}) error {
	key, err := h.data.keyFunc(obj)
	glog.Errorf("heap-add-1 s%", key)
	if err != nil {
		return cache.KeyError{Obj: obj, Err: err}
	}
	if _, exists := h.data.items[key]; exists {
		glog.Errorf("heap-add-2")
		h.data.items[key].obj = obj
		heap.Fix(h.data, h.data.items[key].index)
	} else {
		glog.Errorf("heap-add-3")
		heap.Push(h.data, &itemKeyValue{key, obj})
	}
	return nil
}

// AddIfNotPresent inserts an item, and puts it in the queue. If an item with
// the key is present in the map, no changes is made to the item.
func (h *Heap) AddIfNotPresent(obj interface{}) error {
	key, err := h.data.keyFunc(obj)
	if err != nil {
		return cache.KeyError{Obj: obj, Err: err}
	}
	if _, exists := h.data.items[key]; !exists {
		heap.Push(h.data, &itemKeyValue{key, obj})
	}
	return nil
}

// Update is the same as Add in this implementation. When the item does not
// exist, it is added.
func (h *Heap) Update(obj interface{}) error {
	return h.Add(obj)
}

// Delete removes an item.
func (h *Heap) Delete(obj interface{}) error {
	key, err := h.data.keyFunc(obj)
	if err != nil {
		return cache.KeyError{Obj: obj, Err: err}
	}
	if item, ok := h.data.items[key]; ok {
		heap.Remove(h.data, item.index)
		return nil
	}
	return fmt.Errorf("object not found")
}

// Pop returns the head of the heap.
func (h *Heap) Pop() (interface{}, error) {
	glog.Errorf("heap-Pop-1")
	obj := heap.Pop(h.data)
	if obj != nil {
		return obj, nil
	}
	glog.Errorf("heap-Pop-2")
	return nil, fmt.Errorf("object was removed from heap data")
}

// Get returns the requested item, or sets exists=false.
func (h *Heap) Get(obj interface{}) (interface{}, bool, error) {
	key, err := h.data.keyFunc(obj)
	if err != nil {
		return nil, false, cache.KeyError{Obj: obj, Err: err}
	}
	return h.GetByKey(key)
}

// GetByKey returns the requested item, or sets exists=false.
func (h *Heap) GetByKey(key string) (interface{}, bool, error) {
	item, exists := h.data.items[key]
	if !exists {
		return nil, false, nil
	}
	return item.obj, true, nil
}

// List returns a list of all the items.
func (h *Heap) List() []interface{} {
	list := make([]interface{}, 0, len(h.data.items))
	for _, item := range h.data.items {
		list = append(list, item.obj)
	}
	return list
}

// newHeap returns a Heap which can be used to queue up items to process.
func newHeap(keyFn KeyFunc, lessFn LessFunc) *Heap {
	return &Heap{
		data: &heapData{
			items:    map[string]*heapItem{},
			queue:    []string{},
			keyFunc:  keyFn,
			lessFunc: lessFn,
		},
	}
}
