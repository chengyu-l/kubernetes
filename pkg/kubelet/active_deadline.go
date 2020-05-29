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

package kubelet

import (
	"fmt"
	"time"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/clock"
	"k8s.io/client-go/tools/record"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	"k8s.io/kubernetes/pkg/kubelet/status"
)

const (
	reason  = "DeadlineExceeded"
	message = "Pod was active on the node longer than the specified deadline"
)

// activeDeadlineHandler knows how to enforce active deadlines on pods.
// activeDeadlineHandler知道如何在pod上强制执行活动的截止日期。
type activeDeadlineHandler struct {
	// the clock to use for deadline enforcement
	// 截止日期
	clock clock.Clock
	// the provider of pod status
	podStatusProvider status.PodStatusProvider
	// the recorder to dispatch events when we identify a pod has exceeded active deadline
	recorder record.EventRecorder
}

// newActiveDeadlineHandler returns an active deadline handler that can enforce pod active deadline
func newActiveDeadlineHandler(
	podStatusProvider status.PodStatusProvider,
	recorder record.EventRecorder,
	clock clock.Clock,
) (*activeDeadlineHandler, error) {

	// check for all required fields
	if clock == nil || podStatusProvider == nil || recorder == nil {
		return nil, fmt.Errorf("required arguments must not be nil: %v, %v, %v", clock, podStatusProvider, recorder)
	}
	return &activeDeadlineHandler{
		clock:             clock,
		podStatusProvider: podStatusProvider,
		recorder:          recorder,
	}, nil
}

// ShouldSync returns true if the pod is past its active deadline.
func (m *activeDeadlineHandler) ShouldSync(pod *v1.Pod) bool {
	return m.pastActiveDeadline(pod)
}

// ShouldEvict returns true if the pod is past its active deadline.
// It dispatches an event that the pod should be evicted if it is past its deadline.
func (m *activeDeadlineHandler) ShouldEvict(pod *v1.Pod) lifecycle.ShouldEvictResponse {
	if !m.pastActiveDeadline(pod) {
		return lifecycle.ShouldEvictResponse{Evict: false}
	}
	m.recorder.Eventf(pod, v1.EventTypeNormal, reason, message)
	return lifecycle.ShouldEvictResponse{Evict: true, Reason: reason, Message: message}
}

// pastActiveDeadline returns true if the pod has been active for more than its ActiveDeadlineSeconds
// 如果Pod的活动时长已经超过ActiveDeadlineSeconds，则返回true
func (m *activeDeadlineHandler) pastActiveDeadline(pod *v1.Pod) bool {
	/**
	与Kubernetes的Job有关

	当Job完成时，不再创建任何pod，但也不会删除这些pod。保留这些pod，你就仍然查看已完成pod的日志，以检查错误、警告或其
	他诊断输出。Job对象也会在它完成后保留，以便你可以查看它的状态。用户可以在注意到旧Job的状态后删除它们。使用 kubectl删除
	Job(如： kubectl delete jobs/pi or kubectl delete -f ./job.yaml)。当使用 kubectl删除Job时，它创建的所有pod也会被删除。

	默认情况下，Job将不间断地运行，除非Pod失败，此时Job将遵循上述.spec.backoffLimit 。另一种终止Job的方法是设置活动截止日期。
	通过设置Job的 .spec.activeDeadlineSeconds 字段来实现这一点，单位为：秒。

	activeDeadlineSeconds适用于Job期间，无论有多少个Pod被创建。一时Job达到了 activeDeadlineSeconds，它所有的pod都会被终止，
	并且Job状态变为 type: Failed 、 reason: DeadlineExceeded。

	注意Job的 .spec.activeDeadlineSeconds优先于它的.spec.backoffLimit。因此，重试一个或多个失败的pod的Job在达到
	activeDeadlineSeconds指定的时间限制后将不会部署额外的pod，即使还没有达到 backoffLimit。

	http://www.coderdocument.com/docs/kubernetes/v1.14/concepts/workloads/controllers/jobs_run_to_completion.html
	*/

	// no active deadline was specified
	if pod.Spec.ActiveDeadlineSeconds == nil {
		return false
	}

	// get the latest status to determine if it was started
	// 查询最新的Pod status
	podStatus, ok := m.podStatusProvider.GetPodStatus(pod.UID)
	if !ok {
		podStatus = pod.Status
	}

	// we have no start time so just return
	if podStatus.StartTime.IsZero() {
		return false
	}

	// determine if the deadline was exceeded
	start := podStatus.StartTime.Time
	duration := m.clock.Since(start)
	allowedDuration := time.Duration(*pod.Spec.ActiveDeadlineSeconds) * time.Second
	return duration >= allowedDuration
}
