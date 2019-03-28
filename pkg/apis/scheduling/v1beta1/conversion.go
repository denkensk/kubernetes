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

package v1beta1

import (
	"k8s.io/api/scheduling/v1beta1"
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/kubernetes/pkg/apis/scheduling"
	"strconv"
)

func Convert_scheduling_PriorityClass_To_v1beta1_PriorityClass(in *scheduling.PriorityClass, out *v1beta1.PriorityClass, s conversion.Scope) error {
	if err := autoConvert_scheduling_PriorityClass_To_v1beta1_PriorityClass(in, out, s); err != nil {
		return err
	}
	if in.Preempting != nil {
		if out.Annotations == nil {
			out.Annotations = make(map[string]string)
		}
		out.Annotations[scheduling.PreemptingAnnotation] = strconv.FormatBool(*in.Preempting)
	}
	return nil
}

func Convert_v1beta1_PriorityClass_To_scheduling_PriorityClass(in *v1beta1.PriorityClass, out *scheduling.PriorityClass, s conversion.Scope) error {
	if err := autoConvert_v1beta1_PriorityClass_To_scheduling_PriorityClass(in, out, s); err != nil {
		return err
	}
	if in.Annotations != nil {
		Preempting, err := strconv.ParseBool(in.Annotations[scheduling.PreemptingAnnotation])
		if err != nil {
			return err
		}
		out.Preempting = &Preempting
	}
	return nil
}
