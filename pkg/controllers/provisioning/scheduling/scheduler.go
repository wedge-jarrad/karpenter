/*
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

package scheduling

import (
	"context"
	"fmt"
	"sort"

	"github.com/samber/lo"
	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter/pkg/apis/provisioning/v1alpha5"
	"github.com/aws/karpenter/pkg/cloudprovider"
	"github.com/aws/karpenter/pkg/controllers/state"
	"github.com/aws/karpenter/pkg/events"
	"github.com/aws/karpenter/pkg/scheduling"
	"github.com/aws/karpenter/pkg/utils/resources"
)

func NewScheduler(nodeTemplates []*scheduling.NodeTemplate, provisioners []v1alpha5.Provisioner, cluster *state.Cluster, topology *Topology, instanceTypes []cloudprovider.InstanceType, daemonOverhead map[*scheduling.NodeTemplate]v1.ResourceList, recorder events.Recorder) *Scheduler {
	sort.Slice(instanceTypes, func(i, j int) bool { return instanceTypes[i].Price() < instanceTypes[j].Price() })
	s := &Scheduler{
		nodeTemplates:      nodeTemplates,
		topology:           topology,
		cluster:            cluster,
		instanceTypes:      instanceTypes,
		daemonOverhead:     daemonOverhead,
		recorder:           recorder,
		preferences:        &Preferences{},
		remainingResources: map[string]v1.ResourceList{},
	}

	namedNodeTemplates := lo.KeyBy(s.nodeTemplates, func(nodeTemplate *scheduling.NodeTemplate) string {
		return nodeTemplate.Requirements.Get(v1alpha5.ProvisionerNameLabelKey).Values().List()[0]
	})

	for _, provisioner := range provisioners {
		if provisioner.Spec.Limits != nil {
			s.remainingResources[provisioner.Name] = provisioner.Spec.Limits.Resources
		}
	}

	// create our in-flight nodes
	s.cluster.ForEachNode(func(node *state.Node) bool {
		name, ok := node.Node.Labels[v1alpha5.ProvisionerNameLabelKey]
		if !ok {
			// ignoring this node as it wasn't launched by us
			return true
		}
		nodeTemplate, ok := namedNodeTemplates[name]
		if !ok {
			// ignoring this node as it wasn't launched by a provisioner that we recognize
			return true
		}
		s.inflight = append(s.inflight, NewInFlightNode(node, s.topology, nodeTemplate.StartupTaints, s.daemonOverhead[nodeTemplate]))

		// We don't use the status field and instead recompute the remaining resources to ensure we have a consistent view
		// of the cluster during scheduling.  Depending on how node creation falls out, this will also work for cases where
		// we don't create Node resources.
		s.remainingResources[name] = resources.Subtract(s.remainingResources[name], node.Capacity)
		return true
	})
	return s
}

type Scheduler struct {
	nodes              []*Node
	inflight           []*InFlightNode
	nodeTemplates      []*scheduling.NodeTemplate
	remainingResources map[string]v1.ResourceList // provisioner name -> remaining resources for that provisioner
	instanceTypes      []cloudprovider.InstanceType
	daemonOverhead     map[*scheduling.NodeTemplate]v1.ResourceList
	preferences        *Preferences
	topology           *Topology
	cluster            *state.Cluster
	recorder           events.Recorder
}

func (s *Scheduler) Solve(ctx context.Context, pods []*v1.Pod) ([]*Node, error) {
	// We loop and retrying to schedule to unschedulable pods as long as we are making progress.  This solves a few
	// issues including pods with affinity to another pod in the batch. We could topo-sort to solve this, but it wouldn't
	// solve the problem of scheduling pods where a particular order is needed to prevent a max-skew violation. E.g. if we
	// had 5xA pods and 5xB pods were they have a zonal topology spread, but A can only go in one zone and B in another.
	// We need to schedule them alternating, A, B, A, B, .... and this solution also solves that as well.
	errors := map[*v1.Pod]error{}
	q := NewQueue(pods...)
	for {
		// Try the next pod
		pod, ok := q.Pop()
		if !ok {
			break
		}

		// Schedule to existing nodes or create a new node
		if errors[pod] = s.add(pod); errors[pod] == nil {
			continue
		}

		// If unsuccessful, relax the pod and recompute topology
		relaxed := s.preferences.Relax(ctx, pod)
		q.Push(pod, relaxed)
		if relaxed {
			if err := s.topology.Update(ctx, pod); err != nil {
				logging.FromContext(ctx).Errorf("updating topology, %s", err)
			}
		}
	}
	s.recordSchedulingResults(ctx, q.List(), errors)
	return s.nodes, nil
}

func (s *Scheduler) recordSchedulingResults(ctx context.Context, failedToSchedule []*v1.Pod, errors map[*v1.Pod]error) {
	// notify users of pods that can schedule to inflight capacity
	existingCount := 0
	for _, node := range s.inflight {
		existingCount += len(node.Pods)
		for _, pod := range node.Pods {
			s.recorder.PodShouldSchedule(pod, node.Node)
		}
	}
	newCount := 0
	for _, node := range s.nodes {
		newCount += len(node.Pods)
	}
	if existingCount != 0 || newCount != 0 {
		logging.FromContext(ctx).Infof("%d pod(s) will schedule against new capacity, %d pod(s) against existing capacity", newCount, existingCount)
	}

	// Any remaining pods have failed to schedule
	for _, pod := range failedToSchedule {
		logging.FromContext(ctx).With("pod", client.ObjectKeyFromObject(pod)).Error(errors[pod])
		s.recorder.PodFailedToSchedule(pod, errors[pod])
	}
}

func (s *Scheduler) add(pod *v1.Pod) error {
	// first try to schedule against an in-flight real node
	for _, node := range s.inflight {
		if err := node.Add(pod); err == nil {
			return nil
		}
	}

	// Consider using https://pkg.go.dev/container/heap
	sort.Slice(s.nodes, func(a, b int) bool { return len(s.nodes[a].Pods) < len(s.nodes[b].Pods) })

	// Pick existing node that we are about to create
	for _, node := range s.nodes {
		if err := node.Add(pod); err == nil {
			return nil
		}
	}

	// Create new node
	var errs error
	for _, nodeTemplate := range s.nodeTemplates {
		instanceTypes := s.instanceTypes
		// if limits have been applied to the provisioner, ensure we filter instance types to avoid violating those limits
		if remaining, ok := s.remainingResources[nodeTemplate.ProvisionerName]; ok {
			instanceTypes = filterByRemainingResources(s.instanceTypes, remaining)
			if len(instanceTypes) == 0 {
				errs = multierr.Append(errs, fmt.Errorf("all available instance types exceed provisioner limits"))
				continue
			}
		}

		node := NewNode(nodeTemplate, s.topology, s.daemonOverhead[nodeTemplate], instanceTypes)
		err := node.Add(pod)
		if err == nil {
			s.nodes = append(s.nodes, node)
			// we will launch this node and need to track its maximum possible resource usage against our remaining resources
			s.remainingResources[nodeTemplate.ProvisionerName] = subtractMax(s.remainingResources[nodeTemplate.ProvisionerName], node.InstanceTypeOptions)
			return nil
		}
		errs = multierr.Append(errs, err)
	}
	return errs
}

// subtractMax returns the remaining resources after subtracting the max resource quantity per instance type. To avoid
// overshooting out, we need to pessimistically assume that if e.g. we request a 2, 4 or 8 CPU instance type
// that the 8 CPU instance type is all that will be available.  This could cause a batch of pods to take multiple rounds
// to schedule.
func subtractMax(remaining v1.ResourceList, instanceTypes []cloudprovider.InstanceType) v1.ResourceList {
	// shouldn't occur, but to be safe
	if len(instanceTypes) == 0 {
		return remaining
	}
	var allInstanceResources []v1.ResourceList
	for _, it := range instanceTypes {
		allInstanceResources = append(allInstanceResources, it.Resources())
	}
	result := v1.ResourceList{}
	itResources := resources.MaxResources(allInstanceResources...)
	for k, v := range remaining {
		cp := v.DeepCopy()
		cp.Sub(itResources[k])
		result[k] = cp
	}
	return result
}

// filterByRemainingResources is used to filter out instance types that if launched would exceed the provisioner limits
func filterByRemainingResources(instanceTypes []cloudprovider.InstanceType, remaining v1.ResourceList) []cloudprovider.InstanceType {
	var filtered []cloudprovider.InstanceType
	for _, it := range instanceTypes {
		itResources := it.Resources()
		viableInstance := true
		for resourceName, remainingQuantity := range remaining {
			// if the instance capacity is greater than the remaining quantity for this resource
			if resources.Cmp(itResources[resourceName], remainingQuantity) > 0 {
				viableInstance = false
			}
		}
		if viableInstance {
			filtered = append(filtered, it)
		}
	}
	return filtered
}
