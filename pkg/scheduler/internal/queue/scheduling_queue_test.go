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

package queue

import (
	"reflect"
	"testing"

	"container/list"
	"fmt"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/scheduler/core/equivalence"
	"sync"
)

var negPriority, lowPriority, midPriority, highPriority, veryHighPriority = int32(-100), int32(0), int32(100), int32(1000), int32(10000)
var mediumPriority = (lowPriority + highPriority) / 2
var highPriorityPod1, highPriorityPod2, highPriNominatedPod1, highPriNominatedPod2, medPriorityPod1,
	medPriorityPod2, unschedulablePod1, unschedulablePod2 = v1.Pod{
	ObjectMeta: metav1.ObjectMeta{
		Name:            "hpp1",
		Namespace:       "ns1",
		UID:             "hpp1ns1",
		OwnerReferences: getOwnerReferences("high"),
	},
	Spec: v1.PodSpec{
		Priority: &highPriority,
	},
},
	v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "hpp2",
			Namespace:       "ns1",
			UID:             "hpp2ns1",
			OwnerReferences: getOwnerReferences("high"),
		},
		Spec: v1.PodSpec{
			Priority: &highPriority,
		},
	},
	v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "hpp1",
			Namespace:       "ns1",
			UID:             "hpp1ns1",
			OwnerReferences: getOwnerReferences("high-Nominated"),
		},
		Spec: v1.PodSpec{
			Priority: &highPriority,
		},
		Status: v1.PodStatus{
			NominatedNodeName: "node1",
		},
	},
	v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "hpp2",
			Namespace:       "ns1",
			UID:             "hpp2ns1",
			OwnerReferences: getOwnerReferences("high-Nominated"),
		},
		Spec: v1.PodSpec{
			Priority: &highPriority,
		},
		Status: v1.PodStatus{
			NominatedNodeName: "node1",
		},
	},
	v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mpp1",
			Namespace: "ns2",
			UID:       "mpp1ns2",
			Annotations: map[string]string{
				"annot2": "val2",
			},
			OwnerReferences: getOwnerReferences("medium"),
		},
		Spec: v1.PodSpec{
			Priority: &mediumPriority,
		},
		Status: v1.PodStatus{
			NominatedNodeName: "node1",
		},
	},
	v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mpp2",
			Namespace: "ns2",
			UID:       "mpp2ns2",
			Annotations: map[string]string{
				"annot2": "val2",
			},
			OwnerReferences: getOwnerReferences("medium"),
		},
		Spec: v1.PodSpec{
			Priority: &mediumPriority,
		},
		Status: v1.PodStatus{
			NominatedNodeName: "node1",
		},
	},
	v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "up1",
			Namespace: "ns1",
			UID:       "up1ns1",
			Annotations: map[string]string{
				"annot2": "val2",
			},
			OwnerReferences: getOwnerReferences("low"),
		},
		Spec: v1.PodSpec{
			Priority: &lowPriority,
		},
		Status: v1.PodStatus{
			Conditions: []v1.PodCondition{
				{
					Type:   v1.PodScheduled,
					Status: v1.ConditionFalse,
					Reason: v1.PodReasonUnschedulable,
				},
			},
			NominatedNodeName: "node1",
		},
	},
	v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "up2",
			Namespace: "ns1",
			UID:       "up2ns1",
			Annotations: map[string]string{
				"annot2": "val2",
			},
			OwnerReferences: getOwnerReferences("low"),
		},
		Spec: v1.PodSpec{
			Priority: &lowPriority,
		},
		Status: v1.PodStatus{
			Conditions: []v1.PodCondition{
				{
					Type:   v1.PodScheduled,
					Status: v1.ConditionFalse,
					Reason: v1.PodReasonUnschedulable,
				},
			},
			NominatedNodeName: "node1",
		},
	}

func getOwnerReferences(name string) []metav1.OwnerReference {
	metaOwnerReferences := make([]metav1.OwnerReference, 0)
	metaOwnerReferences = append(metaOwnerReferences, metav1.OwnerReference{
		Kind:       "Job",
		Name:       name,
		UID:        "0eadea6c-e897-11e8-8123-6c92bf3affc9",
		APIVersion: "batch/v1",
	})
	return metaOwnerReferences
}

func clean(p *PriorityQueue) {
	equivalence.CleanEquivalenceCache()
}

func TestPriorityQueue_Add(t *testing.T) {
	q := NewPriorityQueue()
	defer clean(q)

	q.Add(&medPriorityPod1)
	q.Add(&unschedulablePod1)
	q.Add(&highPriorityPod1)
	q.Add(&medPriorityPod2)
	q.Add(&unschedulablePod2)
	q.Add(&highPriorityPod2)
	expectedNominatedPods := map[string][]*v1.Pod{
		"node1": {&medPriorityPod1, &unschedulablePod1, &medPriorityPod2, &unschedulablePod2},
	}

	if !reflect.DeepEqual(q.nominatedPods, expectedNominatedPods) {
		t.Errorf("Unexpected nominated map after adding pods. Expected: %v, got: %v", expectedNominatedPods, q.nominatedPods)
	}

	p, err := q.Pop()
	if err != nil || p != &highPriorityPod1 {
		t.Errorf("Expected: %v after Pop, but got: %v", highPriorityPod1.Name, p.Name)
	}
	l := q.equivalenceCache.Cache[equivalence.GetEquivHash(p)].PodList
	if l.Front().Value.(*v1.Pod) != &highPriorityPod1 {
		t.Errorf("Expected: %v after Pop, but got: %v", highPriorityPod1.Name, l.Front().Value.(*v1.Pod).Name)
	}
	if l.Front().Next().Value.(*v1.Pod) != &highPriorityPod2 {
		t.Errorf("Expected: %v after Pop, but got: %v", highPriorityPod2.Name, l.Front().Next().Value.(*v1.Pod).Name)
	}

	p, err = q.Pop()
	if err != nil || p != &medPriorityPod1 {
		t.Errorf("Expected: %v after Pop, but got: %v", medPriorityPod1.Name, p.Name)
	}
	l = q.equivalenceCache.Cache[equivalence.GetEquivHash(p)].PodList
	if l.Front().Value.(*v1.Pod) != &medPriorityPod1 {
		t.Errorf("Expected: %v after Pop, but got: %v", medPriorityPod1.Name, l.Front().Value.(*v1.Pod).Name)
	}
	if l.Front().Next().Value.(*v1.Pod) != &medPriorityPod2 {
		t.Errorf("Expected: %v after Pop, but got: %v", medPriorityPod2.Name, l.Front().Next().Value.(*v1.Pod).Name)
	}

	p, err = q.Pop()
	if err != nil || p != &unschedulablePod1 {
		t.Errorf("Expected: %v after Pop, but got: %v", unschedulablePod1.Name, p.Name)
	}
	l = q.equivalenceCache.Cache[equivalence.GetEquivHash(p)].PodList
	if l.Front().Value.(*v1.Pod) != &unschedulablePod1 {
		t.Errorf("Expected: %v after Pop, but got: %v", unschedulablePod1.Name, l.Front().Value.(*v1.Pod).Name)
	}
	if l.Front().Next().Value.(*v1.Pod) != &unschedulablePod2 {
		t.Errorf("Expected: %v after Pop, but got: %v", unschedulablePod2.Name, l.Front().Next().Value.(*v1.Pod).Name)
	}

	if len(q.nominatedPods) != 0 {
		t.Errorf("Expected nomindatePods to be empty: %v", q.nominatedPods)
	}
}

func TestPriorityQueue_AddIfNotPresent(t *testing.T) {
	q := NewPriorityQueue()
	defer clean(q)

	eClass := equivalence.NewClass(&highPriNominatedPod1)
	q.equivalenceCache.Cache[eClass.Hash] = eClass
	eClass.PodList.PushBack(&highPriNominatedPod1)
	q.unschedulableQ.addOrUpdate(eClass)

	eClass = equivalence.NewClass(&highPriNominatedPod2)
	q.equivalenceCache.Cache[eClass.Hash] = eClass
	eClass.PodList.PushBack(&highPriNominatedPod2)
	q.unschedulableQ.addOrUpdate(eClass)

	q.AddIfNotPresent(&highPriNominatedPod1) // Must not add anything.
	q.AddIfNotPresent(&medPriorityPod1)
	q.AddIfNotPresent(&unschedulablePod1)
	q.AddIfNotPresent(&highPriNominatedPod2) // Must not add anything.
	q.AddIfNotPresent(&medPriorityPod2)
	q.AddIfNotPresent(&unschedulablePod2)
	expectedNominatedPods := map[string][]*v1.Pod{
		"node1": {&medPriorityPod1, &unschedulablePod1, &medPriorityPod2, &unschedulablePod2},
	}

	if !reflect.DeepEqual(q.nominatedPods, expectedNominatedPods) {
		t.Errorf("Unexpected nominated map after adding pods. Expected: %v, got: %v", expectedNominatedPods, q.nominatedPods)
	}

	p, err := q.Pop()
	if err != nil || p != &medPriorityPod1 {
		t.Errorf("Expected: %v after Pop, but got: %v", medPriorityPod1.Name, p.Name)
	}
	l := q.equivalenceCache.Cache[equivalence.GetEquivHash(p)].PodList
	if l.Front().Value.(*v1.Pod) != &medPriorityPod1 {
		t.Errorf("Expected: %v after Pop, but got: %v", medPriorityPod1.Name, l.Front().Value.(*v1.Pod).Name)
	}
	if l.Front().Next().Value.(*v1.Pod) != &medPriorityPod2 {
		t.Errorf("Expected: %v after Pop, but got: %v", medPriorityPod2.Name, l.Front().Next().Value.(*v1.Pod).Name)
	}

	p, err = q.Pop()
	if err != nil || p != &unschedulablePod1 {
		t.Errorf("Expected: %v after Pop, but got: %v", unschedulablePod1.Name, p.Name)
	}
	l = q.equivalenceCache.Cache[equivalence.GetEquivHash(p)].PodList
	if l.Front().Value.(*v1.Pod) != &unschedulablePod1 {
		t.Errorf("Expected: %v after Pop, but got: %v", unschedulablePod1.Name, l.Front().Value.(*v1.Pod).Name)
	}
	if l.Front().Next().Value.(*v1.Pod) != &unschedulablePod2 {
		t.Errorf("Expected: %v after Pop, but got: %v", unschedulablePod2.Name, l.Front().Next().Value.(*v1.Pod).Name)
	}

	if len(q.nominatedPods) != 0 {
		t.Errorf("Expected nomindatePods to be empty: %v", q.nominatedPods)
	}

	l = q.unschedulableQ.get(equivalence.NewClass(&highPriNominatedPod1)).PodList
	if l.Len() != 2 {
		t.Errorf("Expected highPriNominatedPod PodList len: 2, but got %d.", l.Len())
	}
	if l.Front().Value.(*v1.Pod) != &highPriNominatedPod1 {
		t.Errorf("Expected: %v after Pop, but got: %v", highPriNominatedPod1.Name, l.Front().Value.(*v1.Pod).Name)
	}
	if l.Front().Next().Value.(*v1.Pod) != &highPriNominatedPod2 {
		t.Errorf("Expected: %v after Pop, but got: %v", highPriNominatedPod2.Name, l.Front().Next().Value.(*v1.Pod).Name)
	}
}

func TestPriorityQueue_AddUnschedulableIfNotPresent(t *testing.T) {
	q := NewPriorityQueue()
	defer clean(q)

	q.Add(&highPriNominatedPod1)
	q.Add(&highPriNominatedPod2)
	q.AddUnschedulableIfNotPresent(&highPriNominatedPod1) // Must not add anything.
	q.AddUnschedulableIfNotPresent(&medPriorityPod1)      // This should go to activeQ.
	q.AddUnschedulableIfNotPresent(&unschedulablePod1)
	q.AddUnschedulableIfNotPresent(&highPriNominatedPod2) // Must not add anything.
	q.AddUnschedulableIfNotPresent(&medPriorityPod2)      // This should go to activeQ.
	q.AddUnschedulableIfNotPresent(&unschedulablePod2)

	expectedNominatedPods := map[string][]*v1.Pod{
		"node1": {&highPriNominatedPod1, &highPriNominatedPod2, &medPriorityPod1, &unschedulablePod1, &medPriorityPod2, &unschedulablePod2},
	}
	if !reflect.DeepEqual(q.nominatedPods, expectedNominatedPods) {
		t.Errorf("Unexpected nominated map after adding pods. Expected: %v, got: %v", expectedNominatedPods, q.nominatedPods)
	}
	p, err := q.Pop()
	if err != nil || p != &highPriNominatedPod1 {
		t.Errorf("Expected: %v after Pop, but got: %v", highPriNominatedPod1.Name, p.Name)
	}
	l := q.equivalenceCache.Cache[equivalence.GetEquivHash(p)].PodList
	if l.Front().Value.(*v1.Pod) != &highPriNominatedPod1 {
		t.Errorf("Expected: %v after Pop, but got: %v", highPriNominatedPod1.Name, l.Front().Value.(*v1.Pod).Name)
	}
	if l.Front().Next().Value.(*v1.Pod) != &highPriNominatedPod2 {
		t.Errorf("Expected: %v after Pop, but got: %v", highPriNominatedPod2.Name, l.Front().Next().Value.(*v1.Pod).Name)
	}

	p, err = q.Pop()
	if err != nil || p != &medPriorityPod1 {
		t.Errorf("Expected: %v after Pop, but got: %v", medPriorityPod1.Name, p.Name)
	}
	l = q.equivalenceCache.Cache[equivalence.GetEquivHash(p)].PodList
	if l.Front().Value.(*v1.Pod) != &medPriorityPod1 {
		t.Errorf("Expected: %v after Pop, but got: %v", medPriorityPod1.Name, l.Front().Value.(*v1.Pod).Name)
	}
	if l.Front().Next().Value.(*v1.Pod) != &medPriorityPod2 {
		t.Errorf("Expected: %v after Pop, but got: %v", medPriorityPod2.Name, l.Front().Next().Value.(*v1.Pod).Name)
	}

	l = q.unschedulableQ.get(equivalence.NewClass(&unschedulablePod1)).PodList
	if l.Len() != 2 {
		t.Errorf("Expected highPriNominatedPod PodList len: 2, but got %d.", l.Len())
	}
	if l.Front().Value.(*v1.Pod) != &unschedulablePod1 {
		t.Errorf("Expected: %v after Pop, but got: %v", unschedulablePod1.Name, l.Front().Value.(*v1.Pod).Name)
	}
	if l.Front().Next().Value.(*v1.Pod) != &unschedulablePod2 {
		t.Errorf("Expected: %v after Pop, but got: %v", unschedulablePod2.Name, l.Front().Next().Value.(*v1.Pod).Name)
	}
}

func TestPriorityQueue_Pop(t *testing.T) {
	q := NewPriorityQueue()
	defer clean(q)

	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		p, err := q.Pop()
		if err != nil || p != &medPriorityPod1 {
			t.Errorf("Expected: %v after Pop, but got: %v", medPriorityPod1.Name, p.Name)
		}
		l := q.equivalenceCache.Cache[equivalence.GetEquivHash(p)].PodList
		if l.Front().Value.(*v1.Pod) != &medPriorityPod1 {
			t.Errorf("Expected: %v after Pop, but got: %v", medPriorityPod1.Name, l.Front().Value.(*v1.Pod).Name)
		}
		if l.Front().Next().Value.(*v1.Pod) != &medPriorityPod2 {
			t.Errorf("Expected: %v after Pop, but got: %v", medPriorityPod2.Name, l.Front().Next().Value.(*v1.Pod).Name)
		}
		if len(q.nominatedPods) != 0 {
			t.Errorf("Expected nomindatePods to be empty: %v", q.nominatedPods)
		}
	}()
	q.Add(&medPriorityPod1)
	q.Add(&medPriorityPod2)
	wg.Wait()
}

func TestPriorityQueue_Update(t *testing.T) {
	q := NewPriorityQueue()
	defer clean(q)

	q.Update(nil, &highPriorityPod1)
	if _, exists, _ := q.activeQ.Get(equivalence.NewClass(&highPriorityPod1)); !exists {
		t.Errorf("Expected %v to be added to activeQ.", highPriorityPod1.Name)
	}
	l := equivalence.NewClass(&highPriorityPod1).PodList
	if l.Len() != 1 || l.Front().Value.(*v1.Pod) != &highPriorityPod1 {
		t.Errorf("Expected %v to be added to activeQ.", highPriorityPod1.Name)
	}

	if len(q.nominatedPods) != 0 {
		t.Errorf("Expected nomindatePods to be empty: %v", q.nominatedPods)
	}
	// Update highPriorityPod and add a nominatedNodeName to it.
	q.Update(&highPriorityPod1, &highPriNominatedPod1)
	if q.activeQ.data.Len() != 1 {
		t.Error("Expected only one item in activeQ.")
	}
	if len(q.nominatedPods) != 1 {
		t.Errorf("Expected one item in nomindatePods map: %v", q.nominatedPods)
	}

	// Updating an unschedulable pod which is not in any of the two queues, should
	// add the pod to activeQ.
	q.Update(&unschedulablePod1, &unschedulablePod1)
	if _, exists, _ := q.activeQ.Get(equivalence.NewClass(&unschedulablePod1)); !exists {
		t.Errorf("Expected %v to be added to activeQ.", unschedulablePod1.Name)
	}

	// Updating a pod that is already in activeQ, should not change it.
	q.Update(&unschedulablePod1, &unschedulablePod1)
	if len(q.unschedulableQ.equivalenceClasses) != 0 {
		t.Error("Expected unschedulableQ to be empty.")
	}
	if _, exists, _ := q.activeQ.Get(equivalence.NewClass(&unschedulablePod1)); !exists {
		t.Errorf("Expected: %v to be added to activeQ.", unschedulablePod1.Name)
	}
	if p, err := q.Pop(); err != nil || p != &highPriNominatedPod1 {
		t.Errorf("Expected: %v after Pop, but got: %v", highPriorityPod1.Name, p.Name)
	}
}

func TestPriorityQueue_Delete(t *testing.T) {
	q := NewPriorityQueue()
	defer clean(q)

	q.Update(&highPriorityPod1, &highPriNominatedPod1)
	q.Add(&unschedulablePod1)
	q.Delete(&highPriNominatedPod1)
	if _, exists, _ := q.activeQ.Get(equivalence.NewClass(&unschedulablePod1)); !exists {
		t.Errorf("Expected %v to be in activeQ.", unschedulablePod1.Name)
	}
	if _, exists, _ := q.activeQ.Get(equivalence.NewClass(&highPriNominatedPod1)); exists {
		t.Errorf("Didn't expect %v to be in activeQ.", highPriNominatedPod1.Name)
	}
	if len(q.nominatedPods) != 1 {
		t.Errorf("Expected nomindatePods to have only 'unschedulablePod': %v", q.nominatedPods)
	}
	q.Delete(&unschedulablePod1)
	if len(q.nominatedPods) != 0 {
		t.Errorf("Expected nomindatePods to be empty: %v", q.nominatedPods)
	}
}

func TestPriorityQueue_MoveAllToActiveQueue(t *testing.T) {
	q := NewPriorityQueue()
	defer clean(q)

	q.Add(&medPriorityPod1)

	eClass := equivalence.NewClass(&unschedulablePod1)
	q.equivalenceCache.Cache[eClass.Hash] = eClass
	eClass.PodList.PushBack(&unschedulablePod1)
	q.unschedulableQ.addOrUpdate(eClass)

	eClass = equivalence.NewClass(&highPriorityPod1)
	q.equivalenceCache.Cache[eClass.Hash] = eClass
	eClass.PodList.PushBack(&highPriorityPod1)
	q.unschedulableQ.addOrUpdate(eClass)

	q.MoveAllToActiveQueue()
	if q.activeQ.data.Len() != 3 {
		t.Error("Expected all items to be in activeQ.")
	}
}

// TestPriorityQueue_AssignedPodAdded tests AssignedPodAdded. It checks that
// when a pod with pod affinity is in unschedulableQ and another pod with a
// matching label is added, the unschedulable pod is moved to activeQ.
func TestPriorityQueue_AssignedPodAdded(t *testing.T) {
	affinityPod := unschedulablePod1.DeepCopy()
	affinityPod.Name = "afp"
	affinityPod.OwnerReferences = getOwnerReferences("affinity")
	affinityPod.Spec = v1.PodSpec{
		Affinity: &v1.Affinity{
			PodAffinity: &v1.PodAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
					{
						LabelSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "service",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"securityscan", "value2"},
								},
							},
						},
						TopologyKey: "region",
					},
				},
			},
		},
		Priority: &mediumPriority,
	}
	labelPod := v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lbp",
			Namespace: affinityPod.Namespace,
			Labels:    map[string]string{"service": "securityscan"},
		},
		Spec: v1.PodSpec{NodeName: "machine1"},
	}

	q := NewPriorityQueue()
	q.Add(&medPriorityPod1)

	// Add a couple of pods to the unschedulableQ.
	eClassUnschedulable := equivalence.NewClass(&unschedulablePod1)
	eClassUnschedulable.PodList.PushBack(&unschedulablePod1)
	q.equivalenceCache.Cache[eClassUnschedulable.Hash] = eClassUnschedulable
	q.unschedulableQ.addOrUpdate(eClassUnschedulable)

	eClassAffinity := equivalence.NewClass(affinityPod)
	eClassAffinity.PodList.PushBack(affinityPod)
	q.equivalenceCache.Cache[eClassAffinity.Hash] = eClassAffinity
	q.unschedulableQ.addOrUpdate(eClassAffinity)

	// Simulate addition of an assigned pod. The pod has matching labels for
	// affinityPod. So, affinityPod should go to activeQ.
	q.AssignedPodAdded(&labelPod)

	if q.unschedulableQ.get(eClassAffinity) != nil {
		t.Error("affinityPod is still in the unschedulableQ.")
	}
	if _, exists, _ := q.activeQ.Get(eClassAffinity); !exists {
		t.Error("affinityPod is not moved to activeQ.")
	}
	// Check that the other pod is still in the unschedulableQ.
	if q.unschedulableQ.get(eClassUnschedulable) == nil {
		t.Error("unschedulablePod is not in the unschedulableQ.")
	}
}

func TestPriorityQueue_WaitingPodsForNode(t *testing.T) {
	q := NewPriorityQueue()
	defer clean(q)

	q.Add(&medPriorityPod1)
	q.Add(&unschedulablePod1)
	q.Add(&highPriorityPod1)
	q.Add(&medPriorityPod2)
	q.Add(&unschedulablePod2)
	q.Add(&highPriorityPod2)

	p, err := q.Pop()
	if err != nil || p != &highPriorityPod1 {
		t.Errorf("Expected: %v after Pop, but got: %v", highPriorityPod1.Name, p.Name)
	}
	l := q.equivalenceCache.Cache[equivalence.GetEquivHash(p)].PodList
	if l.Front().Value.(*v1.Pod) != &highPriorityPod1 {
		t.Errorf("Expected: %v after Pop, but got: %v", highPriorityPod1.Name, l.Front().Value.(*v1.Pod).Name)
	}
	if l.Front().Next().Value.(*v1.Pod) != &highPriorityPod2 {
		t.Errorf("Expected: %v after Pop, but got: %v", highPriorityPod2.Name, l.Front().Next().Value.(*v1.Pod).Name)
	}

	expectedList := []*v1.Pod{&medPriorityPod1, &unschedulablePod1, &medPriorityPod2, &unschedulablePod2}
	if !reflect.DeepEqual(expectedList, q.WaitingPodsForNode("node1")) {
		t.Error("Unexpected list of nominated Pods for node.")
		t.Errorf("Expected: %v , but got: %v", expectedList, q.WaitingPodsForNode("node1"))
	}
	if q.WaitingPodsForNode("node2") != nil {
		t.Error("Expected list of nominated Pods for node2 to be empty.")
	}
}

/*
func TestPriorityQueue_WaitingPods(t *testing.T) {
	q := NewPriorityQueue()
	defer clean(q)

	q.Add(&unschedulablePod1)
	q.Add(&highPriorityPod1)
	q.Add(&medPriorityPod1)
	q.Add(&unschedulablePod2)
	q.Add(&highPriorityPod2)
	q.Add(&medPriorityPod2)

	p, err := q.Pop()
	if err != nil || p != &highPriorityPod1 {
		t.Errorf("Expected: %v after Pop, but got: %v", highPriorityPod1.Name, p.Name)
	}
	l := q.equivalenceCache.Cache[equivalence.GetEquivHash(p)].PodList
	if l.Front().Value.(*v1.Pod) != &highPriorityPod1 {
		t.Errorf("Expected: %v after Pop, but got: %v", highPriorityPod1.Name, l.Front().Value.(*v1.Pod).Name)
	}
	if l.Front().Next().Value.(*v1.Pod) != &highPriorityPod2 {
		t.Errorf("Expected: %v after Pop, but got: %v", highPriorityPod2.Name, l.Front().Next().Value.(*v1.Pod).Name)
	}

	expectedList := []*v1.Pod{&unschedulablePod1, &unschedulablePod2, &medPriorityPod1, &medPriorityPod2}
	if !reflect.DeepEqual(expectedList, q.WaitingPods()) {
		t.Error("Unexpected list of nominated Pods for node.")
		t.Errorf("Expected: %v , but got: %v", expectedList, q.WaitingPods())
	}
}
*/

func TestUnschedulablePodsMap(t *testing.T) {
	q := NewPriorityQueue()
	defer clean(q)

	var pods = []*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "p0",
				Namespace: "ns1",
				Annotations: map[string]string{
					"annot1": "val1",
				},
				OwnerReferences: getOwnerReferences("p0"),
			},
			Status: v1.PodStatus{
				NominatedNodeName: "node1",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "p1",
				Namespace: "ns1",
				Annotations: map[string]string{
					"annot": "val",
				},
				OwnerReferences: getOwnerReferences("p2"),
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "p2",
				Namespace: "ns2",
				Annotations: map[string]string{
					"annot2": "val2", "annot3": "val3",
				},
				OwnerReferences: getOwnerReferences("p3"),
			},
			Status: v1.PodStatus{
				NominatedNodeName: "node3",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "p3",
				Namespace:       "ns4",
				OwnerReferences: getOwnerReferences("p4"),
			},
			Status: v1.PodStatus{
				NominatedNodeName: "node1",
			},
		},
	}
	var updatedPods = make([]*v1.Pod, len(pods))
	updatedPods[0] = pods[0].DeepCopy()
	updatedPods[1] = pods[1].DeepCopy()
	updatedPods[3] = pods[3].DeepCopy()

	//tests := []struct {
	//	name                   string
	//	podsToAdd              []*v1.Pod
	//	expectedMapAfterAdd    map[uint64]*equivalence.Class
	//	podsToUpdate           []*v1.Pod
	//	expectedMapAfterUpdate map[uint64]*equivalence.Class
	//	podsToDelete           []*v1.Pod
	//	expectedMapAfterDelete map[uint64]*equivalence.Class
	//}{
	//	{
	//		name:      "create, update, delete subset of pods",
	//		podsToAdd: []*v1.Pod{pods[0], pods[1], pods[2], pods[3]},
	//		expectedMapAfterAdd: map[uint64]*equivalence.Class{
	//			equivalence.GetEquivHash(pods[0]): getEquivalenceClass(pods[0]),
	//			equivalence.GetEquivHash(pods[1]): getEquivalenceClass(pods[1]),
	//			equivalence.GetEquivHash(pods[2]): getEquivalenceClass(pods[2]),
	//			equivalence.GetEquivHash(pods[3]): getEquivalenceClass(pods[3]),
	//		},
	//		podsToUpdate: []*v1.Pod{updatedPods[0]},
	//		expectedMapAfterUpdate: map[uint64]*equivalence.Class{
	//			equivalence.GetEquivHash(pods[0]): getEquivalenceClass(updatedPods[0]),
	//			equivalence.GetEquivHash(pods[1]): getEquivalenceClass(pods[1]),
	//			equivalence.GetEquivHash(pods[2]): getEquivalenceClass(pods[2]),
	//			equivalence.GetEquivHash(pods[3]): getEquivalenceClass(pods[3]),
	//		},
	//		podsToDelete: []*v1.Pod{pods[0], pods[1]},
	//		expectedMapAfterDelete: map[uint64]*equivalence.Class{
	//			equivalence.GetEquivHash(pods[2]): getEquivalenceClass(pods[2]),
	//			equivalence.GetEquivHash(pods[3]): getEquivalenceClass(pods[3]),
	//		},
	//	},
	//	{
	//		name:      "create, update, delete all",
	//		podsToAdd: []*v1.Pod{pods[0], pods[3]},
	//		expectedMapAfterAdd: map[uint64]*equivalence.Class{
	//			equivalence.GetEquivHash(pods[0]): getEquivalenceClass(pods[0]),
	//			equivalence.GetEquivHash(pods[3]): getEquivalenceClass(pods[3]),
	//		},
	//		podsToUpdate: []*v1.Pod{updatedPods[3]},
	//		expectedMapAfterUpdate: map[uint64]*equivalence.Class{
	//			equivalence.GetEquivHash(pods[0]): getEquivalenceClass(pods[0]),
	//			equivalence.GetEquivHash(pods[3]): getEquivalenceClass(updatedPods[3]),
	//		},
	//		podsToDelete:           []*v1.Pod{pods[0], pods[3]},
	//		expectedMapAfterDelete: map[uint64]*equivalence.Class{},
	//	},
	//	{
	//		name:      "delete non-existing and existing pods",
	//		podsToAdd: []*v1.Pod{pods[1], pods[2]},
	//		expectedMapAfterAdd: map[uint64]*equivalence.Class{
	//			equivalence.GetEquivHash(pods[1]): getEquivalenceClass(pods[1]),
	//			equivalence.GetEquivHash(pods[2]): getEquivalenceClass(pods[2]),
	//		},
	//		podsToUpdate: []*v1.Pod{updatedPods[1]},
	//		expectedMapAfterUpdate: map[uint64]*equivalence.Class{
	//			equivalence.GetEquivHash(pods[0]): getEquivalenceClass(updatedPods[1]),
	//			equivalence.GetEquivHash(pods[2]): getEquivalenceClass(pods[2]),
	//		},
	//		podsToDelete: []*v1.Pod{pods[2], pods[3]},
	//		expectedMapAfterDelete: map[uint64]*equivalence.Class{
	//			equivalence.GetEquivHash(pods[0]): getEquivalenceClass(updatedPods[1]),
	//		},
	//	},
	//}

	podsToAdd := []*v1.Pod{pods[0], pods[1], pods[2], pods[3]}
	for _, p := range podsToAdd {
		eClass := equivalence.NewClass(p)
		eClass.PodList.PushBack(p)
		q.equivalenceCache.Cache[eClass.Hash] = eClass
		q.unschedulableQ.addOrUpdate(eClass)
	}

	expectedMapAfterAdd := map[uint64]*equivalence.Class{
		equivalence.GetEquivHash(pods[0]): getEquivalenceClass(pods[0]),
		equivalence.GetEquivHash(pods[1]): getEquivalenceClass(pods[1]),
		equivalence.GetEquivHash(pods[2]): getEquivalenceClass(pods[2]),
		equivalence.GetEquivHash(pods[3]): getEquivalenceClass(pods[3]),
	}
	if !reflect.DeepEqual(q.unschedulableQ.equivalenceClasses, expectedMapAfterAdd) {
		t.Errorf("Unexpected map after adding pods. Expected: %v, got: %v",
			expectedMapAfterAdd, q.unschedulableQ.equivalenceClasses)
	}

	podsToUpdate := []*v1.Pod{updatedPods[0]}
	if len(podsToUpdate) > 0 {
		for _, p := range podsToUpdate {
			eClass := equivalence.NewClass(p)
			var next *list.Element
			for e := eClass.PodList.Front(); e != nil; e = next {
				if e.Value.(*v1.Pod).Name == p.Name {
					e.Value = p
				}
				next = e.Next()
			}
			q.equivalenceCache.Cache[eClass.Hash] = eClass
			q.unschedulableQ.addOrUpdate(eClass)
		}
		expectedMapAfterUpdate := map[uint64]*equivalence.Class{
			equivalence.GetEquivHash(pods[0]): getEquivalenceClass(updatedPods[0]),
			equivalence.GetEquivHash(pods[1]): getEquivalenceClass(pods[1]),
			equivalence.GetEquivHash(pods[2]): getEquivalenceClass(pods[2]),
			equivalence.GetEquivHash(pods[3]): getEquivalenceClass(pods[3]),
		}
		if !reflect.DeepEqual(q.unschedulableQ.equivalenceClasses, expectedMapAfterUpdate) {
			t.Errorf("Unexpected map after updating pods. Expected: %v, got: %v",
				expectedMapAfterUpdate, q.unschedulableQ.equivalenceClasses)
		}
	}

	podsToDelete := []*v1.Pod{pods[0], pods[1]}
	for _, p := range podsToDelete {
		eClass := equivalence.NewClass(p)
		var next *list.Element
		for e := eClass.PodList.Front(); e != nil; e = next {
			next = e.Next()
			if e.Value.(*v1.Pod).Name == p.Name {
				eClass.PodList.Remove(e)
			}
		}
		q.equivalenceCache.Cache[eClass.Hash] = eClass
		if eClass.PodList.Len() == 0 {
			q.unschedulableQ.delete(eClass)
		}
	}
	expectedMapAfterDelete := map[uint64]*equivalence.Class{
		equivalence.GetEquivHash(pods[2]): getEquivalenceClass(pods[2]),
		equivalence.GetEquivHash(pods[3]): getEquivalenceClass(pods[3]),
	}
	if !reflect.DeepEqual(q.unschedulableQ.equivalenceClasses, expectedMapAfterDelete) {
		t.Errorf("Unexpected map after deleting pods. Expected: %v, got: %v",
			expectedMapAfterDelete, q.unschedulableQ.equivalenceClasses)
	}

	q.unschedulableQ.clear()
	if len(q.unschedulableQ.equivalenceClasses) != 0 {
		t.Errorf("Expected the map to be empty, but has %v elements.", len(q.unschedulableQ.equivalenceClasses))
	}
}

func getEquivalenceClass(p *v1.Pod) *equivalence.Class {
	eClass := equivalence.NewClass(p)
	eClass.PodList.PushBack(p)
	return eClass
}

func TestSchedulingQueue_Close(t *testing.T) {
	tests := []struct {
		name        string
		q           SchedulingQueue
		expectedErr error
	}{
		{
			name:        "FIFO close",
			q:           NewFIFO(),
			expectedErr: fmt.Errorf(queueClosed),
		},
		{
			name:        "PriorityQueue close",
			q:           NewPriorityQueue(),
			expectedErr: fmt.Errorf(queueClosed),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			wg := sync.WaitGroup{}
			wg.Add(1)
			go func() {
				defer wg.Done()
				pod, err := test.q.Pop()
				if err.Error() != test.expectedErr.Error() {
					t.Errorf("Expected err %q from Pop() if queue is closed, but got %q", test.expectedErr.Error(), err.Error())
				}
				if pod != nil {
					t.Errorf("Expected pod nil from Pop() if queue is closed, but got: %v", pod)
				}
			}()
			test.q.Close()
			wg.Wait()
		})
	}
}
