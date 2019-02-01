/*
Copyright 2019 The Kubernetes Authors.

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

// This file contains structures that implement equivalence class types.
// Pods under same controllerRef will be put in the same Class.podSet
// and will be considered as an unit in the process of scheduling.
// Equivalence class improves scheduler velocity and responsiveness by
// continuous scheduling when one is determined schedulable and avoiding checking
// the schedulability of all of these pods when one is determined unschedulable.
// This is particularly useful in batch processing.

package equivalence

import (
	"fmt"
	"sync"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/kubernetes/pkg/scheduler/util"
)

// ClassMap is the type of a map which has the key by a pod's UID of controllerRef and
// the value which is a pointer to the PodClass.
type ClassMap map[types.UID]*PodClass

var (
	once     sync.Once
	classMap ClassMap
)

// NewClassMap creates an empty classMap which is a map key by a pod's UID of
// controllerRef and the value is a pointer to the PodClass.
func NewClassMap() ClassMap {
	// Make sure ClassMap is a singleton
	//once.Do(func() {
	//	classMap = make(ClassMap)
	//})
	classMap = make(ClassMap)
	return classMap
}

// PodClass holds pods under same controllerRef and is the unit in scheduling process.
// of scheduling.
type PodClass struct {
	// Pods's UID of controllerRef
	Hash types.UID
	// Pods which under same controllerRef waiting to be scheduled.
	PodSet *sync.Map
}

// NewClass returns equivalence class if class exists in classMap and creates a new equivalence class if not exists.
func (cm ClassMap) EquivClass(pod *v1.Pod) *PodClass {
	equivHash := GetEquivHash(pod)

	if podClass := cm[equivHash]; podClass == nil {
		cm[equivHash] = &PodClass{
			Hash:   equivHash,
			PodSet: new(sync.Map),
		}
	}

	return classMap[equivHash]
}

// GetEquivHash returns the pod's UID of controllerRef.
// NOTE (resouer): To avoid hash collision issue, in alpha stage we decide to use `controllerRef` only to determine
// whether two pods are equivalent. This may change with the feature evolves.
func GetEquivHash(pod *v1.Pod) types.UID {
	ownerReferences := pod.GetOwnerReferences()
	if ownerReferences != nil {
		return ownerReferences[0].UID
	}

	// If pod's UID of controllerRef is nil, return pod.UID.
	return pod.UID
}

// AddPod adds the pod in PodClass.PodSet and returns the pointer to PodClass.
func (c *PodClass) AddPod(pod *v1.Pod) *PodClass {
	c.PodSet.Store(pod.UID, pod)
	return c
}

// DeletePod deletes the pod in podClass.PodSet and returns true if PodSet is empty or false
// if PodSet isn't empty.
func (c *PodClass) DeletePod(pod *v1.Pod) bool {
	c.PodSet.Delete(pod.UID)
	empty := true
	c.PodSet.Range(func(k, v interface{}) bool {
		empty = false
		return false
	})
	return empty
}

// UpdatePod updates the pod from old to new and returns the point to PodClass.
func (c *PodClass) UpdatePod(old *v1.Pod, new *v1.Pod) *PodClass {
	c.PodSet.Delete(old.UID)
	c.PodSet.Store(new.UID, new)
	return c
}

// ClassKeyFunc is a convenient default KeyFunc which knows how to make
// keys for API objects which implement meta.Interface. The key uses the equivalenceClass.Hash.
func ClassKeyFunc(obj interface{}) (string, error) {
	if obj == nil {
		return "", fmt.Errorf("obj is nil")
	}
	equivalenceClass := obj.(*PodClass)
	hash := equivalenceClass.Hash
	return string(hash), nil
}

// HigherPriorityEquivalenceClass returns true when priority of the first class is higher than
// the second class. It takes arguments of the type "interface{}" to be used with
// SortableList, but expects those arguments to be *PodClass.
func HigherPriorityEquivalenceClass(class1, class2 interface{}) bool {
	var pod1 *v1.Pod
	var pod2 *v1.Pod

	class1.(*PodClass).PodSet.Range(func(k, v interface{}) bool {
		if v != nil {
			pod1 = v.(*v1.Pod)
			return false
		}
		return true
	})
	class2.(*PodClass).PodSet.Range(func(k, v interface{}) bool {
		if v != nil {
			pod2 = v.(*v1.Pod)
			return false
		}
		return true
	})

	if pod1 == nil {
		return false
	} else if pod2 == nil {
		return true
	} else {
		return util.GetPodPriority(pod1) > util.GetPodPriority(pod2)
	}
}
