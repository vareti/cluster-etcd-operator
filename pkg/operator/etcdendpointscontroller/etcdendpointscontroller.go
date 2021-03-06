package etcdendpointscontroller

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	operatorv1helpers "github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
)

// EtcdEndpointsController maintains a configmap resource with
// IP addresses for etcd. It should never depend on DNS directly or transitively.
type EtcdEndpointsController struct {
	operatorClient  v1helpers.OperatorClient
	nodeLister      corev1listers.NodeLister
	configmapLister corev1listers.ConfigMapLister
	configmapClient corev1client.ConfigMapsGetter
}

func NewEtcdEndpointsController(
	operatorClient v1helpers.OperatorClient,
	eventRecorder events.Recorder,
	kubeClient kubernetes.Interface,
	kubeInformers operatorv1helpers.KubeInformersForNamespaces,
) factory.Controller {
	kubeInformersForTargetNamespace := kubeInformers.InformersFor(operatorclient.TargetNamespace)
	configmapsInformer := kubeInformersForTargetNamespace.Core().V1().ConfigMaps()
	kubeInformersForCluster := kubeInformers.InformersFor("")
	nodeInformer := kubeInformersForCluster.Core().V1().Nodes()

	c := &EtcdEndpointsController{
		operatorClient:  operatorClient,
		nodeLister:      nodeInformer.Lister(),
		configmapLister: configmapsInformer.Lister(),
		configmapClient: kubeClient.CoreV1(),
	}
	return factory.New().ResyncEvery(time.Minute).WithInformers(
		operatorClient.Informer(),
		configmapsInformer.Informer(),
		nodeInformer.Informer(),
	).WithSync(c.sync).ToController("EtcdEndpointsController", eventRecorder.WithComponentSuffix("etcd-endpoints-controller"))
}

func (c *EtcdEndpointsController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	err := c.syncConfigMap(ctx, syncCtx.Recorder())

	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdEndpointsDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "ErrorUpdatingEtcdEndpoints",
			Message: err.Error(),
		}))
		if updateErr != nil {
			syncCtx.Recorder().Warning("EtcdEndpointsErrorUpdatingStatus", updateErr.Error())
		}
		return err
	}

	_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
		Type:   "EtcdEndpointsDegraded",
		Status: operatorv1.ConditionFalse,
		Reason: "EtcdEndpointsUpdated",
	}))
	if updateErr != nil {
		syncCtx.Recorder().Warning("EtcdEndpointsErrorUpdatingStatus", updateErr.Error())
		return updateErr
	}
	return nil
}

func (c *EtcdEndpointsController) syncConfigMap(ctx context.Context, recorder events.Recorder) error {
	required := configMapAsset()

	// create endpoint addresses for each node
	nodes, err := c.nodeLister.List(labels.Set{"node-role.kubernetes.io/master": ""}.AsSelector())
	if err != nil {
		return fmt.Errorf("unable to list expected etcd member nodes: %v", err)
	}
	endpointAddresses := map[string]string{}
	for _, node := range nodes {
		var nodeInternalIP string
		for _, nodeAddress := range node.Status.Addresses {
			if nodeAddress.Type == corev1.NodeInternalIP {
				nodeInternalIP = nodeAddress.Address
				break
			}
		}
		if len(nodeInternalIP) == 0 {
			return fmt.Errorf("unable to determine internal ip address for node %s", node.Name)
		}
		endpointAddresses[base64.StdEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(nodeInternalIP))] = nodeInternalIP
	}

	if len(endpointAddresses) == 0 {
		return fmt.Errorf("no master nodes are present")
	}

	required.Data = endpointAddresses

	_, _, err = resourceapply.ApplyConfigMap(c.configmapClient, recorder, required)
	return err
}

func configMapAsset() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "etcd-endpoints",
			Namespace: operatorclient.TargetNamespace,
		},
	}
}
