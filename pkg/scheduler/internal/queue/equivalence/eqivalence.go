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
// EquivClass is a thread safe map of unschedulable equivalence hashes.
// Once a pod is marked an unschedulable, its equivalence hash is added
// to the map. Equivalence class improves scheduler velocity and responsiveness
// by avoiding checking the schedulability of all of these pods when one is
// determined unschedulable. This is particularly useful in batch processing.

package equivalence

import (
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"sync"
)

// EquivClass is a thread safe map saves the Previous scheduling information,
// it uses equivHash as key to access those flagPodUID. FlagPodUID is the uid
// of the first failed Pod.
type EquivClass struct {
	equivClassMap map[types.UID]types.UID
	lock          sync.RWMutex
}

// New creates an empty EquivClass.
func New() *EquivClass {
	return &EquivClass{
		equivClassMap: make(map[types.UID]types.UID),
	}
}

// Add adds the podFlagUID with the equivHash to the equivClassMap.
func (e *EquivClass) Add(equivHash types.UID, flagPodUID types.UID) {
	e.lock.Lock()
	defer e.lock.Unlock()
	e.equivClassMap[equivHash] = flagPodUID
}

// Get gets the podFlagUID with the equivHash from the equivClassMap，
func (e *EquivClass) Get(equivHash types.UID) (types.UID, bool) {
	e.lock.RLock()
	defer e.lock.RUnlock()
	flagPodUID, ok := e.equivClassMap[equivHash]
	return flagPodUID, ok
}

// Delete deletes the podFlagUID with the equivHash from the equivClassMap.
func (e *EquivClass) Delete(equivHash types.UID) {
	e.lock.Lock()
	defer e.lock.Unlock()
	delete(e.equivClassMap, equivHash)
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
