/*
Copyright 2016 The Kubernetes Authors.

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

// Package equivalence defines Pod equivalence classes and the equivalence class
// cache.
package equivalence

import (
	"sync"

	"container/list"
	"fmt"
	"github.com/golang/glog"
	"hash/fnv"
	"k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/scheduler/util"
	hashutil "k8s.io/kubernetes/pkg/util/hash"
	"strconv"
)

var equivalenceCache *EquivalenceCache

type EquivalenceCache struct {
	Cache map[uint64]*Class
}

type Class struct {
	// Lock to protect class
	Mu *sync.RWMutex
	// Equivalence hash
	Hash uint64
	// Pods wait for schedule in this Class
	PodList *list.List
}

func NewClass(pod *v1.Pod) *Class {
	equivHash := GetEquivHash(pod)
	if equivalenceCache == nil {
		equivalenceCache = NewEquivalenceCache()
	}
	if _, ok := equivalenceCache.Cache[equivHash]; ok {
		return equivalenceCache.Cache[equivHash]
	}
	//equivalencePod := getEquivalencePod(pod)
	//if equivalencePod != nil {
	//	hash := fnv.New32a()
	//	hashutil.DeepHashObject(hash, equivalencePod)
	//	return &Class{
	//		Hash:    equivHash,
	//		PodList: list.New(),
	//		Mu:      new(sync.RWMutex),
	//	}
	//}

	return &Class{
		Hash:    equivHash,
		PodList: list.New(),
		Mu:      new(sync.RWMutex),
	}
}

/*
// EquivalenceCache is a thread safe map saves and reuses the output of predicate
// it uses hash as key to access those cached results.
type EquivalenceCache struct {
	mu    sync.RWMutex
	Cache map[uint64]types.UID
}
*/

// NewEquivalenceCache create an empty equiv class cache.
func NewEquivalenceCache() *EquivalenceCache {
	if equivalenceCache != nil {
		return equivalenceCache
	}
	cache := make(map[uint64]*Class)
	equivalenceCache = &EquivalenceCache{
		Cache: cache,
	}
	return equivalenceCache
}

func CleanEquivalenceCache() {
	equivalenceCache = nil
}

/*
// Invalidate clears cached for the given hash.
func (c *EquivalenceCache) Invalidate(hash uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.Cache, hash)
}
*/

// GetEquivHash generate EquivHash for Pod
func GetEquivHash(pod *v1.Pod) uint64 {
	hash := fnv.New32a()
	ownerReferences := pod.GetOwnerReferences()
	if ownerReferences != nil {
		hashutil.DeepHashObject(hash, ownerReferences[0])
	} else {
		equivalencePod := getEquivalencePod(pod)
		if equivalencePod != nil {
			hashutil.DeepHashObject(hash, equivalencePod)
		}
	}

	return uint64(hash.Sum32())
}

// be considered equivalent for scheduling purposes. For correctness, this must
// include any Pod field which is used by a FitPredicate.
//
// NOTE: For equivalence hash to be formally correct, lists and maps in the
// equivalencePod should be normalized. (e.g. by sorting them) However, the vast
// majority of equivalent pod classes are expected to be created from a single
// pod template, so they will all have the same ordering.
type equivalencePod struct {
	Namespace      *string
	Labels         map[string]string
	Affinity       *v1.Affinity
	Containers     []v1.Container // See note about ordering
	InitContainers []v1.Container // See note about ordering
	NodeName       *string
	NodeSelector   map[string]string
	Tolerations    []v1.Toleration
	Volumes        []v1.Volume // See note about ordering
}

// getEquivalencePod returns a normalized representation of a pod so that two
// "equivalent" pods will hash to the same value.
func getEquivalencePod(pod *v1.Pod) *equivalencePod {
	ep := &equivalencePod{
		Namespace:      &pod.Namespace,
		Labels:         pod.Labels,
		Affinity:       pod.Spec.Affinity,
		Containers:     pod.Spec.Containers,
		InitContainers: pod.Spec.InitContainers,
		NodeName:       &pod.Spec.NodeName,
		NodeSelector:   pod.Spec.NodeSelector,
		Tolerations:    pod.Spec.Tolerations,
		Volumes:        pod.Spec.Volumes,
	}
	// DeepHashObject considers nil and empty slices to be different. Normalize them.
	if len(ep.Containers) == 0 {
		ep.Containers = nil
	}
	if len(ep.InitContainers) == 0 {
		ep.InitContainers = nil
	}
	if len(ep.Tolerations) == 0 {
		ep.Tolerations = nil
	}
	if len(ep.Volumes) == 0 {
		ep.Volumes = nil
	}
	// Normalize empty maps also.
	if len(ep.Labels) == 0 {
		ep.Labels = nil
	}
	if len(ep.NodeSelector) == 0 {
		ep.NodeSelector = nil
	}
	// TODO(misterikkit): Also normalize nested maps and slices.
	return ep
}

// MetaNamespaceKeyFunc is a convenient default KeyFunc which knows how to make
// keys for API objects which implement meta.Interface.
// The key uses the format <namespace>/<name> unless <namespace> is empty, then
// it's just <name>.
//
// TODO: replace key-as-string with a key-as-struct so that this
// packing/unpacking won't be necessary.
func MetaNamespaceKeyFunc(obj interface{}) (string, error) {
	if obj == nil {
		glog.Error("obj is nil")
		return "", fmt.Errorf("obj is nil")
	}
	equivalenceClass := obj.(*Class)
	hash := (*equivalenceClass).Hash
	glog.Errorf("hash: d%", hash)
	return strconv.Itoa(int(hash)), nil
}

// HigherPriorityPod return true when priority of the first pod is higher than
// the second one. It takes arguments of the type "interface{}" to be used with
// SortableList, but expects those arguments to be *v1.Pod.
func HigherPriorityEquivalenceClass(class1, class2 interface{}) bool {
	pod1 := class1.(*Class).PodList.Front().Value
	pod2 := class2.(*Class).PodList.Front().Value
	return util.GetPodPriority(pod1.(*v1.Pod)) > util.GetPodPriority(pod2.(*v1.Pod))
}
