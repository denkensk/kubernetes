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

package util

import (
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/kubernetes/pkg/apis/scheduling"
	"k8s.io/kubernetes/pkg/features"
)

// DropDisabledFields removes disabled fields from the PriorityClass object.
func DropDisabledFields(class, oldClass *scheduling.PriorityClass) {
	if !utilfeature.DefaultFeatureGate.Enabled(features.NonPreemptingPriority) && !preemptingPriorityInUse(oldClass) {
		class.PreemptionPolicy = ""
	}
}

func preemptingPriorityInUse(oldClass *scheduling.PriorityClass) bool {
	if oldClass == nil {
		return false
	}
	if oldClass.PreemptionPolicy != "" {
		return true
	}
	return false
}
