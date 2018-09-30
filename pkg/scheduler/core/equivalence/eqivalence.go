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
	"hash/fnv"
	"sync"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	hashutil "k8s.io/kubernetes/pkg/util/hash"
)

// EquivalenceCache is a thread safe map saves and reuses the output of predicate
// it uses hash as key to access those cached results.
type EquivalenceCache struct {
	mu    sync.RWMutex
	Cache map[uint64]types.UID
}

// NewEquivalenceCache create an empty equiv class cache.
func NewEquivalenceCache() *EquivalenceCache {
	cache := make(map[uint64]types.UID)
	return &EquivalenceCache{
		Cache: cache,
	}
}

// Invalidate clears cached for the given hash.
func (c *EquivalenceCache) Invalidate(hash uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.Cache, hash)
}

// GetEquivHash generate EquivHash for Pod
func (c *EquivalenceCache) GetEquivHash(pod *v1.Pod) uint64 {
	hash := fnv.New32a()
	ownerReferences := pod.GetOwnerReferences()
	if ownerReferences != nil {
		hashutil.DeepHashObject(hash, ownerReferences)
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
