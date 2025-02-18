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

package state

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"knative.dev/pkg/logging"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const nodeControllerName = "node-state"

// NodeController reconciles nodes for the purpose of maintaining state regarding nodes that is expensive to compute.
type NodeController struct {
	kubeClient client.Client
	cluster    *Cluster
}

// NewNodeController constructs a controller instance
func NewNodeController(kubeClient client.Client, cluster *Cluster) *NodeController {
	return &NodeController{
		kubeClient: kubeClient,
		cluster:    cluster,
	}
}

func (c *NodeController) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).Named(nodeControllerName).With("node", req.Name))
	node := &v1.Node{}
	if err := c.kubeClient.Get(ctx, req.NamespacedName, node); err != nil {
		if errors.IsNotFound(err) {
			// notify cluster state of the node deletion
			c.cluster.deleteNode(req.Name)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}
	// ensure it's aware of any nodes we discover, this is a no-op if the node is already known to our cluster state
	c.cluster.updateNode(node)

	return reconcile.Result{Requeue: true, RequeueAfter: stateRetryPeriod}, nil
}

func (c *NodeController) Register(ctx context.Context, m manager.Manager) error {
	return controllerruntime.
		NewControllerManagedBy(m).
		Named(nodeControllerName).
		For(&v1.Node{}).
		Complete(c)
}
