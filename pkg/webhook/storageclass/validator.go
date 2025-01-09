package storageclass

import (
	"fmt"
	"slices"

	"github.com/harvester/webhook/pkg/server/admission"
	ctlcorev1 "github.com/rancher/wrangler/v3/pkg/generated/controllers/core/v1"
	ctlstoragev1 "github.com/rancher/wrangler/v3/pkg/generated/controllers/storage/v1"
	admissionregv1 "k8s.io/api/admissionregistration/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"

	"github.com/harvester/csi-driver-lvm/pkg/utils"
)

type Validator struct {
	admission.DefaultValidator

	storageClassCtrl ctlstoragev1.StorageClassController
	nodeCtrl         ctlcorev1.NodeController
}

func NewStorageClassValidator(storageClassCtrl ctlstoragev1.StorageClassController, nodeCtrl ctlcorev1.NodeController) *Validator {
	return &Validator{
		storageClassCtrl: storageClassCtrl,
		nodeCtrl:         nodeCtrl,
	}
}

func (v *Validator) Create(_ *admission.Request, newObj runtime.Object) error {
	if err := v.validateTopologyNodes(newObj); err != nil {
		return err
	}
	return v.validateUniqueVGType(newObj)
}

func (v *Validator) Update(_ *admission.Request, oldObj runtime.Object, newObj runtime.Object) error {
	// We only need to care the topology node should not be changed
	oldSc := oldObj.(*storagev1.StorageClass)
	newSc := newObj.(*storagev1.StorageClass)
	if getLVMTopologyNodes(oldSc) != getLVMTopologyNodes(newSc) {
		return fmt.Errorf("storageclass %s with provisioner %s, topology node can not be changed", newSc.Name, newSc.Provisioner)
	}
	return nil
}

func (v *Validator) validateTopologyNodes(obj runtime.Object) error {
	sc := obj.(*storagev1.StorageClass)
	klog.Infof("Validating storage class %s, provisioner: %v", sc.Name, sc.Provisioner)
	if sc.Provisioner != utils.LVMCSIDriver {
		return nil
	}
	if sc.AllowedTopologies == nil || len(sc.AllowedTopologies) == 0 {
		return fmt.Errorf("storageclass %s with provisioner %s must have allowedTopologies", sc.Name, sc.Provisioner)
	}
	nodes, err := v.nodeCtrl.List(metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list nodes: %v, we need node list for validation", err)
	}
	nodeList := []string{}
	for _, node := range nodes.Items {
		nodeList = append(nodeList, node.Name)
	}
	foundMatchTopologyKey := false
	for _, topology := range sc.AllowedTopologies {
		if topology.MatchLabelExpressions == nil || len(topology.MatchLabelExpressions) == 0 {
			return fmt.Errorf("storageclass %s with provisioner %s must have MatchLabelExpressions in allowedTopologies", sc.Name, sc.Provisioner)
		}
		for _, matchLabel := range topology.MatchLabelExpressions {
			if matchLabel.Key == utils.LVMTopologyNodeKey {
				foundMatchTopologyKey = true
				// LVM can only support one node
				if matchLabel.Values == nil || len(matchLabel.Values) != 1 {
					return fmt.Errorf("storageclass %s with provisioner %s must have one values in MatchLabelExpressions", sc.Name, sc.Provisioner)
				}
				if !slices.Contains(nodeList, matchLabel.Values[0]) {
					return fmt.Errorf("storageclass %s with provisioner %s has invalid node name %s", sc.Name, sc.Provisioner, matchLabel.Values[0])
				}
			}
		}
	}
	if !foundMatchTopologyKey {
		return fmt.Errorf("storageclass %s with provisioner %s must have MatchLabelExpressions with key %s", sc.Name, sc.Provisioner, utils.LVMTopologyNodeKey)
	}

	return nil
}

func (v *Validator) validateUniqueVGType(obj runtime.Object) error {
	sc := obj.(*storagev1.StorageClass)
	klog.Infof("Validating storage class %s, provisioner: %v", sc.Name, sc.Provisioner)
	if sc.Provisioner != utils.LVMCSIDriver {
		return nil
	}
	creatingVGType := sc.Parameters["type"]
	creatingVGName := sc.Parameters["vgName"]
	// must have, we have validated in validateTopologyNodes first.
	creatingNodeName := getLVMTopologyNodes(sc)
	klog.Infof("Creating VG type: %s, name: %s, node: %s", creatingVGType, creatingVGName, creatingNodeName)
	if creatingVGType == "" || creatingVGName == "" || creatingNodeName == "" {
		return fmt.Errorf("storageclass %s with provisioner %s must have type, vgName and topology node", sc.Name, sc.Provisioner)
	}
	scList, err := v.storageClassCtrl.List(metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, storageClass := range scList.Items {
		klog.Infof("Checking the SC: %v", storageClass)
		scCpy := storageClass.DeepCopy()
		if storageClass.Provisioner != utils.LVMCSIDriver {
			continue
		}
		if storageClass.Name == sc.Name {
			continue
		}
		targetNode := getLVMTopologyNodes(scCpy)
		if targetNode != creatingNodeName {
			continue
		}
		if storageClass.Parameters["vgName"] != creatingVGName {
			continue
		}
		if storageClass.Parameters["type"] != creatingVGType {
			return fmt.Errorf("storageclass %s with vg Type %s is conflict with storageclass %s vgType(%s), vgName(%s)", sc.Name, creatingVGType, storageClass.Name, storageClass.Parameters["type"], storageClass.Parameters["vgName"])
		}
	}
	return nil
}

func (v *Validator) Resource() admission.Resource {
	return admission.Resource{
		Names:      []string{"storageclasses"},
		Scope:      admissionregv1.ClusterScope,
		APIGroup:   storagev1.SchemeGroupVersion.Group,
		APIVersion: storagev1.SchemeGroupVersion.Version,
		ObjectType: &storagev1.StorageClass{},
		OperationTypes: []admissionregv1.OperationType{
			admissionregv1.Create,
			admissionregv1.Update,
		},
	}
}

func getLVMTopologyNodes(sc *storagev1.StorageClass) string {
	for _, topology := range sc.AllowedTopologies {
		for _, matchLabel := range topology.MatchLabelExpressions {
			if matchLabel.Key == utils.LVMTopologyNodeKey {
				return matchLabel.Values[0]
			}
		}
	}
	return ""
}
