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

This file was copied and modified from the kubernetes/kubernetes project
https://github.com/kubernetes/kubernetes/release-1.8/pkg/controller/deployment/sync.go

Modifications Copyright SAP SE or an SAP affiliate company and Gardener contributors
*/

// Package controller is used to provide the core functionalities of machine-controller-manager
package controller

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strconv"

	"github.com/gardener/machine-controller-manager/pkg/apis/machine/v1alpha1"
	labelsutil "github.com/gardener/machine-controller-manager/pkg/util/labels"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/klog/v2"
)

// syncStatusOnly only updates Deployments Status and doesn't take any mutating actions.
func (dc *controller) syncStatusOnly(ctx context.Context, d *v1alpha1.MachineDeployment, isList []*v1alpha1.MachineSet, machineMap map[types.UID]*v1alpha1.MachineList) error {
	newIS, oldISs, err := dc.getAllMachineSetsAndSyncRevision(ctx, d, isList, machineMap, false)
	if err != nil {
		return err
	}

	allISs := append(oldISs, newIS)
	return dc.syncMachineDeploymentStatus(ctx, allISs, newIS, d)
}

// sync is responsible for reconciling deployments on scaling events or when they
// are paused.
func (dc *controller) sync(ctx context.Context, d *v1alpha1.MachineDeployment, isList []*v1alpha1.MachineSet, machineMap map[types.UID]*v1alpha1.MachineList) error {
	newIS, oldISs, err := dc.getAllMachineSetsAndSyncRevision(ctx, d, isList, machineMap, false)
	if err != nil {
		return err
	}
	if err := dc.scale(ctx, d, newIS, oldISs); err != nil {
		// If we get an error while trying to scale, the deployment will be requeued
		// so we can abort this resync
		return err
	}

	// Clean up the deployment when it's paused and no rollback is in flight.
	if d.Spec.Paused && d.Spec.RollbackTo == nil {
		if err := dc.cleanupMachineDeployment(ctx, oldISs, d); err != nil {
			return err
		}
	}

	allISs := append(oldISs, newIS)
	return dc.syncMachineDeploymentStatus(ctx, allISs, newIS, d)
}

// checkPausedConditions checks if the given deployment is paused or not and adds an appropriate condition.
// These conditions are needed so that we won't accidentally report lack of progress for resumed deployments
// that were paused for longer than progressDeadlineSeconds.
func (dc *controller) checkPausedConditions(ctx context.Context, d *v1alpha1.MachineDeployment) error {
	cond := GetMachineDeploymentCondition(d.Status, v1alpha1.MachineDeploymentProgressing)
	if cond != nil && cond.Reason == TimedOutReason {
		// If we have reported lack of progress, do not overwrite it with a paused condition.
		return nil
	}
	pausedCondExists := cond != nil && cond.Reason == PausedMachineDeployReason

	needsUpdate := false
	if d.Spec.Paused && !pausedCondExists {
		condition := NewMachineDeploymentCondition(v1alpha1.MachineDeploymentProgressing, v1alpha1.ConditionUnknown, PausedMachineDeployReason, "Deployment is paused")
		SetMachineDeploymentCondition(&d.Status, *condition)
		needsUpdate = true
	} else if !d.Spec.Paused && pausedCondExists {
		condition := NewMachineDeploymentCondition(v1alpha1.MachineDeploymentProgressing, v1alpha1.ConditionUnknown, ResumedMachineDeployReason, "Deployment is resumed")
		SetMachineDeploymentCondition(&d.Status, *condition)
		needsUpdate = true
	}

	if !needsUpdate {
		return nil
	}

	_, err := dc.controlMachineClient.MachineDeployments(d.Namespace).UpdateStatus(ctx, d, metav1.UpdateOptions{})
	return err
}

// getAllMachineSetsAndSyncRevision returns all the machine sets for the provided deployment (new and all old), with new MS's and deployment's revision updated.
//
// rsList should come from getReplicaSetsForDeployment(d).
// machineMap should come from getmachineMapForDeployment(d, rsList).
//
//  1. Get all old RSes this deployment targets, and calculate the max revision number among them (maxOldV).
//  2. Get new RS this deployment targets (whose machine template matches deployment's), and update new RS's revision number to (maxOldV + 1),
//     only if its revision number is smaller than (maxOldV + 1). If this step failed, we'll update it in the next deployment sync loop.
//  3. Copy new RS's revision number to deployment (update deployment's revision). If this step failed, we'll update it in the next deployment sync loop.
//
// Note that currently the deployment controller is using caches to avoid querying the server for reads.
// This may lead to stale reads of machine sets, thus incorrect deployment status.
func (dc *controller) getAllMachineSetsAndSyncRevision(ctx context.Context, d *v1alpha1.MachineDeployment, isList []*v1alpha1.MachineSet, machineMap map[types.UID]*v1alpha1.MachineList, createIfNotExisted bool) (*v1alpha1.MachineSet, []*v1alpha1.MachineSet, error) {
	// List the deployment's RSes & machines and apply machine-template-hash info to deployment's adopted RSes/machines
	isList, err := dc.isAndMachinesWithHashKeySynced(ctx, d, isList, machineMap)
	if err != nil {
		return nil, nil, fmt.Errorf("error labeling machine sets and machine with machine-template-hash: %v", err)
	}
	_, allOldISs := FindOldMachineSets(d, isList)

	// Get new machine set with the updated revision number
	newIS, err := dc.getNewMachineSet(ctx, d, isList, allOldISs, createIfNotExisted)
	if err != nil {
		return nil, nil, err
	}

	return newIS, allOldISs, nil
}

// rsAndmachinesWithHashKeySynced returns the RSes and machines the given deployment
// targets, with machine-template-hash information synced.
//
// rsList should come from getReplicaSetsForDeployment(d).
// machineMap should come from getmachineMapForDeployment(d, rsList).
func (dc *controller) isAndMachinesWithHashKeySynced(ctx context.Context, d *v1alpha1.MachineDeployment, isList []*v1alpha1.MachineSet, machineMap map[types.UID]*v1alpha1.MachineList) ([]*v1alpha1.MachineSet, error) {
	var syncedISList []*v1alpha1.MachineSet
	for _, is := range isList {
		// Add machine-template-hash information if it's not in the RS.
		// Otherwise, new RS produced by Deployment will overlap with pre-existing ones
		// that aren't constrained by the machine-template-hash.
		syncedIS, err := dc.addHashKeyToISAndMachines(ctx, is, machineMap[is.UID], d.Status.CollisionCount)
		if err != nil {
			return nil, err
		}
		syncedISList = append(syncedISList, syncedIS)
	}
	return syncedISList, nil
}

// addHashKeyToRSAndmachines adds machine-template-hash information to the given rs, if it's not already there, with the following steps:
// 1. Add hash label to the rs's machine template, and make sure the controller sees this update so that no orphaned machines will be created
// 2. Add hash label to all machines this rs owns, wait until replicaset controller reports rs.Status.FullyLabeledReplicas equal to the desired number of replicas
// 3. Add hash label to the rs's label and selector
func (dc *controller) addHashKeyToISAndMachines(ctx context.Context, is *v1alpha1.MachineSet, machineList *v1alpha1.MachineList, collisionCount *int32) (*v1alpha1.MachineSet, error) {
	// If the rs already has the new hash label in its selector, it's done syncing
	if labelsutil.SelectorHasLabel(is.Spec.Selector, v1alpha1.DefaultMachineDeploymentUniqueLabelKey) {
		return is, nil
	}
	hash, err := GetMachineSetHash(is, collisionCount)
	if err != nil {
		return nil, err
	}
	// 1. Add hash template label to the rs. This ensures that any newly created machines will have the new label.
	updatedIS, err := UpdateISWithRetries(ctx, dc.controlMachineClient.MachineSets(is.Namespace), dc.machineSetLister, is.Namespace, is.Name,
		func(updated *v1alpha1.MachineSet) error {
			// Precondition: the RS doesn't contain the new hash in its machine template label.
			if updated.Spec.Template.Labels[v1alpha1.DefaultMachineDeploymentUniqueLabelKey] == hash {
				return utilerrors.ErrPreconditionViolated
			}
			updated.Spec.Template.Labels = labelsutil.AddLabel(updated.Spec.Template.Labels, v1alpha1.DefaultMachineDeploymentUniqueLabelKey, hash)
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("error updating machine set %s machine template label with template hash: %v", is.Name, err)
	}
	// Make sure rs machine template is updated so that it won't create machines without the new label (orphaned machines).
	if updatedIS.Generation > updatedIS.Status.ObservedGeneration {
		// TODO: Revisit if we really need to wait here as opposed to returning and
		// potentially unblocking this worker (can wait up to 1min before timing out).
		if err = WaitForMachineSetUpdated(dc.machineSetLister, updatedIS.Generation, updatedIS.Namespace, updatedIS.Name); err != nil {
			return nil, fmt.Errorf("error waiting for machine set %s to be observed by controller: %v", updatedIS.Name, err)
		}
		klog.V(4).Infof("Observed the update of machine set %s's machine template with hash %s.", is.Name, hash)
	}

	// 2. Update all machines managed by the rs to have the new hash label, so they will be correctly adopted.
	if err := LabelMachinesWithHash(ctx, machineList, dc.controlMachineClient, dc.machineLister, is.Namespace, is.Name, hash); err != nil {
		return nil, fmt.Errorf("error in adding template hash label %s to machines %+v: %s", hash, machineList, err)
	}

	// We need to wait for the replicaset controller to observe the machines being
	// labeled with machine template hash. Because previously we've called
	// WaitForReplicaSetUpdated, the replicaset controller should have dropped
	// FullyLabeledReplicas to 0 already, we only need to wait it to increase
	// back to the number of replicas in the spec.
	// TODO: Revisit if we really need to wait here as opposed to returning and
	// potentially unblocking this worker (can wait up to 1min before timing out).
	if err := WaitForMachinesHashPopulated(dc.machineSetLister, updatedIS.Generation, updatedIS.Namespace, updatedIS.Name); err != nil {
		return nil, fmt.Errorf("Machine set %s: error waiting for machineset controller to observe machines being labeled with template hash: %v", updatedIS.Name, err)
	}

	// 3. Update rs label and selector to include the new hash label
	// Copy the old selector, so that we can scrub out any orphaned machines
	updatedIS, err = UpdateISWithRetries(ctx, dc.controlMachineClient.MachineSets(is.Namespace), dc.machineSetLister, is.Namespace, is.Name, func(updated *v1alpha1.MachineSet) error {
		// Precondition: the RS doesn't contain the new hash in its label and selector.
		if updated.Labels[v1alpha1.DefaultMachineDeploymentUniqueLabelKey] == hash && updated.Spec.Selector.MatchLabels[v1alpha1.DefaultMachineDeploymentUniqueLabelKey] == hash {
			return utilerrors.ErrPreconditionViolated
		}
		updated.Labels = labelsutil.AddLabel(updated.Labels, v1alpha1.DefaultMachineDeploymentUniqueLabelKey, hash)
		updated.Spec.Selector = labelsutil.AddLabelToSelector(updated.Spec.Selector, v1alpha1.DefaultMachineDeploymentUniqueLabelKey, hash)
		return nil
	})
	// If the RS isn't actually updated, that's okay, we'll retry in the
	// next sync loop since its selector isn't updated yet.
	if err != nil {
		return nil, fmt.Errorf("error updating MachineSet %s label and selector with template hash: %v", updatedIS.Name, err)
	}

	// TODO: look for orphaned machines and label them in the background somewhere else periodically
	return updatedIS, nil
}

// Returns a machine set that matches the intent of the given deployment. Returns nil if the new machine set doesn't exist yet.
// 1. Get existing new RS (the RS that the given deployment targets, whose machine template is the same as deployment's).
// 2. If there's existing new RS, update its revision number if it's smaller than (maxOldRevision + 1), where maxOldRevision is the max revision number among all old RSes.
// 3. If there's no existing new RS and createIfNotExisted is true, create one with appropriate revision number (maxOldRevision + 1) and replicas.
// Note that the machine-template-hash will be added to adopted RSes and machines.
func (dc *controller) getNewMachineSet(ctx context.Context, d *v1alpha1.MachineDeployment, isList, oldISs []*v1alpha1.MachineSet, createIfNotExisted bool) (*v1alpha1.MachineSet, error) {
	existingNewIS := FindNewMachineSet(d, isList)

	// Calculate the max revision number among all old RSes
	maxOldRevision := MaxRevision(oldISs)
	// Calculate revision number for this new machine set
	newRevision := strconv.FormatInt(maxOldRevision+1, 10)

	// Latest machine set exists. We need to sync its annotations (includes copying all but
	// annotationsToSkip from the parent deployment, and update revision, desiredReplicas,
	// and maxReplicas) and also update the revision annotation in the deployment with the
	// latest revision.
	if existingNewIS != nil {
		isCopy := existingNewIS.DeepCopy()

		// Set existing new machine set's annotation
		annotationsUpdated := SetNewMachineSetAnnotations(d, isCopy, newRevision, true)
		minReadySecondsNeedsUpdate := isCopy.Spec.MinReadySeconds != d.Spec.MinReadySeconds
		nodeTemplateUpdated := SetNewMachineSetNodeTemplate(d, isCopy, newRevision, true)
		machineConfigUpdated := SetNewMachineSetConfig(d, isCopy, newRevision, true)
		updateMachineSetClassKind := UpdateMachineSetClassKind(d, isCopy, newRevision, true)

		if annotationsUpdated || minReadySecondsNeedsUpdate || nodeTemplateUpdated || machineConfigUpdated || updateMachineSetClassKind {
			isCopy.Spec.MinReadySeconds = d.Spec.MinReadySeconds
			return dc.controlMachineClient.MachineSets(isCopy.Namespace).Update(ctx, isCopy, metav1.UpdateOptions{})
		}

		// Should use the revision in existingNewRS's annotation, since it set by before
		needsUpdate := SetMachineDeploymentRevision(d, isCopy.Annotations[RevisionAnnotation])
		cond := GetMachineDeploymentCondition(d.Status, v1alpha1.MachineDeploymentProgressing)
		if cond == nil {
			msg := fmt.Sprintf("Found new machine set %q", isCopy.Name)
			condition := NewMachineDeploymentCondition(v1alpha1.MachineDeploymentProgressing, v1alpha1.ConditionTrue, FoundNewISReason, msg)
			SetMachineDeploymentCondition(&d.Status, *condition)
			needsUpdate = true
		}

		if needsUpdate {
			var err error
			newStatus := d.Status
			if d, err = dc.controlMachineClient.MachineDeployments(d.Namespace).Update(ctx, d, metav1.UpdateOptions{}); err != nil {
				return nil, err
			}
			dCopy := d.DeepCopy()
			dCopy.Status = newStatus
			if _, err = dc.controlMachineClient.MachineDeployments(dCopy.Namespace).UpdateStatus(ctx, dCopy, metav1.UpdateOptions{}); err != nil {
				return nil, err
			}
		}
		return isCopy, nil
	}

	if !createIfNotExisted {
		return nil, nil
	}

	// new MachineSet does not exist, create one.
	newISTemplate := *d.Spec.Template.DeepCopy()
	machineTemplateSpecHash := fmt.Sprintf("%d", ComputeHash(&newISTemplate, d.Status.CollisionCount))
	newISTemplate.Labels = labelsutil.CloneAndAddLabel(d.Spec.Template.Labels, v1alpha1.DefaultMachineDeploymentUniqueLabelKey, machineTemplateSpecHash)
	// Add machineTemplateHash label to selector.
	newISSelector := labelsutil.CloneSelectorAndAddLabel(d.Spec.Selector, v1alpha1.DefaultMachineDeploymentUniqueLabelKey, machineTemplateSpecHash)

	const encodedHashLimit = 5
	encodedMachineTemplateSpecHash := rand.SafeEncodeString(machineTemplateSpecHash)
	if len(encodedMachineTemplateSpecHash) > encodedHashLimit {
		encodedMachineTemplateSpecHash = encodedMachineTemplateSpecHash[:encodedHashLimit]
	}

	// Create new ReplicaSet
	newIS := v1alpha1.MachineSet{
		ObjectMeta: metav1.ObjectMeta{
			// Make the name deterministic, to ensure idempotence
			Name:            d.Name + "-" + encodedMachineTemplateSpecHash,
			Namespace:       d.Namespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(d, controllerKind)},
			Labels:          newISTemplate.Labels,
		},
		Spec: v1alpha1.MachineSetSpec{
			Replicas:        0,
			MinReadySeconds: d.Spec.MinReadySeconds,
			Selector:        newISSelector,
			Template:        newISTemplate,
		},
	}
	allISs := append(oldISs, &newIS)
	newReplicasCount, err := NewISNewReplicas(d, allISs, &newIS)
	if err != nil {
		return nil, err
	}

	newIS.Spec.Replicas = newReplicasCount
	// Set new machine set's annotation
	SetNewMachineSetAnnotations(d, &newIS, newRevision, false)
	// Create the new ReplicaSet. If it already exists, then we need to check for possible
	// hash collisions. If there is any other error, we need to report it in the status of
	// the Deployment.
	alreadyExists := false
	createdIS, err := dc.controlMachineClient.MachineSets(newIS.Namespace).Create(ctx, &newIS, metav1.CreateOptions{})
	switch {
	// We may end up hitting this due to a slow cache or a fast resync of the Deployment.
	// Fetch a copy of the ReplicaSet. If its owner is our deployment and its machineTemplateSpec is semantically deep equal
	// with the machineTemplateSpec of the Deployment, then that is our new ReplicaSet. Otherwise,
	// this is a hash collision and we need to increment the collisionCount field in the
	// status of the Deployment and try the creation again.
	case errors.IsAlreadyExists(err):
		alreadyExists = true
		is, isErr := dc.machineSetLister.MachineSets(newIS.Namespace).Get(newIS.Name)
		if isErr != nil {
			return nil, isErr
		}

		// bought these changes from current latest k/k deployment code (https://github.com/kubernetes/kubernetes/blob/0e19bbb91644885a6db38a77ea3d697730269802/pkg/controller/deployment/sync.go#L240-L243)
		controllerRef := metav1.GetControllerOf(is)
		if controllerRef != nil && controllerRef.UID == d.UID && EqualIgnoreHash(&d.Spec.Template, &is.Spec.Template) {
			// Pass through the matching ReplicaSet as the new ReplicaSet.
			createdIS = is
			err = nil
			break
		}

		// Matching ReplicaSet is not equal - increment the collisionCount in the DeploymentStatus
		// and requeue the Deployment.
		if d.Status.CollisionCount == nil {
			d.Status.CollisionCount = new(int32)
		}
		preCollisionCount := *d.Status.CollisionCount
		*d.Status.CollisionCount++
		// Update the collisionCount for the Deployment and let it requeue by returning the original
		// error.
		_, dErr := dc.controlMachineClient.MachineDeployments(d.Namespace).UpdateStatus(ctx, d, metav1.UpdateOptions{})
		if dErr == nil {
			klog.V(2).Infof("Found a hash collision for machine deployment %q - bumping collisionCount (%d->%d) to resolve it", d.Name, preCollisionCount, *d.Status.CollisionCount)
		}
		return nil, err
	// bought this case from current latest k/k deployment code (https://github.com/kubernetes/kubernetes/blob/0e19bbb91644885a6db38a77ea3d697730269802/pkg/controller/deployment/sync.go#L260-L262)
	case errors.HasStatusCause(err, v1.NamespaceTerminatingCause):
		// if the namespace is terminating, all subsequent creates will fail and we can safely do nothing
		return nil, err
	case err != nil:
		msg := fmt.Sprintf("Failed to create new machine set %q: %v", newIS.Name, err)
		cond := NewMachineDeploymentCondition(v1alpha1.MachineDeploymentProgressing, v1alpha1.ConditionFalse, FailedISCreateReason, msg)
		SetMachineDeploymentCondition(&d.Status, *cond)
		// We don't really care about this error at this point, since we have a bigger issue to report.
		// TODO: Identify which errors are permanent and switch DeploymentIsFailed  to take into account
		// these reasons as well. Related issue: https://github.com/kubernetes/kubernetes/issues/18568
		_, _ = dc.controlMachineClient.MachineDeployments(d.Namespace).UpdateStatus(ctx, d, metav1.UpdateOptions{})
		dc.recorder.Eventf(d, v1.EventTypeWarning, FailedISCreateReason, msg)
		return nil, err
	}
	if !alreadyExists && newReplicasCount > 0 {
		dc.recorder.Eventf(d, v1.EventTypeNormal, "ScalingMachineSet", "Scaled up machine set %s to %d", createdIS.Name, newReplicasCount)
	}

	needsUpdate := SetMachineDeploymentRevision(d, newRevision)
	if !alreadyExists {
		msg := fmt.Sprintf("Created new machine set %q", createdIS.Name)
		condition := NewMachineDeploymentCondition(v1alpha1.MachineDeploymentProgressing, v1alpha1.ConditionTrue, NewMachineSetReason, msg)
		SetMachineDeploymentCondition(&d.Status, *condition)
		needsUpdate = true
	}
	if needsUpdate {
		_, err = dc.controlMachineClient.MachineDeployments(d.Namespace).UpdateStatus(ctx, d, metav1.UpdateOptions{})
	}
	return createdIS, err
}

func (dc *controller) scale(ctx context.Context, deployment *v1alpha1.MachineDeployment, newIS *v1alpha1.MachineSet, oldISs []*v1alpha1.MachineSet) error {
	// If there is only one active machine set then we should scale that up to the full count of the
	// deployment. If there is no active machine set, then we should scale up the newest machine set.
	if activeOrLatest := FindActiveOrLatest(newIS, oldISs); activeOrLatest != nil {
		if (activeOrLatest.Spec.Replicas) == (deployment.Spec.Replicas) {
			// to deal with the case where the DesiredReplicas annotation is outdated (issue - https://github.com/gardener/machine-controller-manager/issues/815)
			klog.V(3).Infof("DesiredReplicas annotation possibly outdated for the machineSet %s, updating if needed...", activeOrLatest.Name)
			_, _, err := dc.scaleMachineSet(ctx, activeOrLatest, deployment.Spec.Replicas, deployment, "no-op")
			return err
		}
		klog.V(3).Infof("Scaling latest/theOnlyActive machineSet %s", activeOrLatest.Name)
		_, _, err := dc.scaleMachineSetAndRecordEvent(ctx, activeOrLatest, deployment.Spec.Replicas, deployment)
		return err
	}

	// If the new machine set is saturated, old machine sets should be fully scaled down.
	// This case handles machine set adoption during a saturated new machine set.
	if IsSaturated(deployment, newIS) {
		klog.V(3).Infof("Scaling old active machineSets as new machineSet %s is saturated", newIS.Name)
		for _, old := range FilterActiveMachineSets(oldISs) {
			if _, _, err := dc.scaleMachineSetAndRecordEvent(ctx, old, 0, deployment); err != nil {
				return err
			}
		}
		return nil
	}

	// There are old machine sets with machines and the new machine set is not saturated.
	// So the scaling is handled this way:
	// - Scale up   ? -> scale up only the new machineSet
	// - Scale down ? -> scale down all active machineSets proportionally
	if IsRollingUpdate(deployment) {
		klog.V(3).Infof("Scaling all active machineSets proportionally for scale-in, while scaling up latest machineSet only for scale-out, machineDeployment %s", deployment.Name)
		allISs := FilterActiveMachineSets(append(oldISs, newIS))
		allISsReplicas := GetReplicaCountForMachineSets(allISs)

		allowedSize := int32(0)
		if (deployment.Spec.Replicas) > 0 {
			allowedSize = (deployment.Spec.Replicas) + MaxSurge(*deployment)
		}

		// Number of additional replicas that can be either added or removed from the total
		// replicas count. These replicas should be distributed proportionally to the active
		// machine sets in case of scale-in, while added only to the new machineSet during scale-out
		deploymentReplicasToAdd := allowedSize - allISsReplicas
		klog.V(3).Infof("machineDeployment: %s , replicasToAdd: %d, maxAllowedSize: %d, allMachineSetReplicas: %d", deployment.Name, deploymentReplicasToAdd, allowedSize, allISsReplicas)

		// During scale-in, the additional replicas should be distributed proportionally amongst the active
		// machine sets from the larger to the smaller in size machine set.
		// We should scale down older machine sets first if machine sets are of equal size.

		var scalingOperation string
		nameToSize := make(map[string]int32)
		deploymentReplicasAdded := int32(0)
		switch {
		case deploymentReplicasToAdd >= 0:
			scalingOperation = "up"
			nameToSize = dc.scaleNewMachineSet(newIS, allISs, deploymentReplicasToAdd, deployment)
			deploymentReplicasAdded = deploymentReplicasToAdd
		case deploymentReplicasToAdd < 0:
			scalingOperation = "down"
			sort.Sort(MachineSetsBySizeOlder(allISs))
			nameToSize, deploymentReplicasAdded = dc.scaleMachineSetsProportionally(allISs, deploymentReplicasToAdd, deployment)
		}

		for i := range allISs {
			is := allISs[i]

			// Incorporate any leftovers to the largest machine set.
			if i == 0 && deploymentReplicasToAdd != 0 {
				leftover := deploymentReplicasToAdd - deploymentReplicasAdded
				nameToSize[is.Name] = nameToSize[is.Name] + leftover
				if nameToSize[is.Name] < 0 {
					nameToSize[is.Name] = 0
				}
				klog.V(3).Infof("leftover proportion increase of %d done in largest machineSet %s", leftover, is.Name)
			}

			// TODO: Use transactions when we have them.
			if _, _, err := dc.scaleMachineSet(ctx, is, nameToSize[is.Name], deployment, scalingOperation); err != nil {
				// Return as soon as we fail, the deployment is requeued
				klog.Warningf("updating machineSet %s failed while scaling. This could lead to desired replicas annotation not being updated. err: %v", is.Name, err)
				return err
			}
		}
	}
	return nil
}

func (dc *controller) scaleNewMachineSet(newIS *v1alpha1.MachineSet, allISs []*v1alpha1.MachineSet, deploymentReplicasToAdd int32, deployment *v1alpha1.MachineDeployment) map[string]int32 {
	nameToSize := make(map[string]int32)
	for _, is := range allISs {
		nameToSize[is.Name] = is.Spec.Replicas
	}

	if newIS != nil {
		nameToSize[newIS.Name] = newIS.Spec.Replicas + deploymentReplicasToAdd
	} else {
		klog.V(3).Infof("New machineSet for machineDeployment %s which needs to be scaled-up is not present", deployment.Name)
	}

	return nameToSize
}

func (dc *controller) scaleMachineSetsProportionally(allISs []*v1alpha1.MachineSet, deploymentReplicasToAdd int32, deployment *v1alpha1.MachineDeployment) (map[string]int32, int32) {
	// Iterate over all active machine sets and estimate proportions for each of them.
	// The absolute value of deploymentReplicasAdded should never exceed the absolute
	// value of deploymentReplicasToAdd.

	nameToSize := make(map[string]int32)
	deploymentReplicasAdded := int32(0)
	for i := range allISs {
		is := allISs[i]
		// Estimate proportions if we have replicas to add, otherwise simply populate
		// nameToSize with the current sizes for each machine set.
		if deploymentReplicasToAdd != 0 {
			proportion := GetProportion(is, *deployment, deploymentReplicasToAdd, deploymentReplicasAdded)
			klog.V(3).Infof("final proportion increase for machineSet %s due to parent deployment's replica update is %d", is.Name, proportion)
			nameToSize[is.Name] = (is.Spec.Replicas) + proportion
			deploymentReplicasAdded += proportion
		} else {
			nameToSize[is.Name] = (is.Spec.Replicas)
		}
	}

	return nameToSize, deploymentReplicasAdded
}

func (dc *controller) scaleMachineSetAndRecordEvent(ctx context.Context, is *v1alpha1.MachineSet, newScale int32, deployment *v1alpha1.MachineDeployment) (bool, *v1alpha1.MachineSet, error) {
	// No need to scale
	if (is.Spec.Replicas) == newScale {
		return false, is, nil
	}
	var scalingOperation string
	if (is.Spec.Replicas) < newScale {
		scalingOperation = "up"
	} else {
		scalingOperation = "down"
	}
	scaled, newIS, err := dc.scaleMachineSet(ctx, is, newScale, deployment, scalingOperation)
	return scaled, newIS, err
}

func (dc *controller) scaleMachineSet(ctx context.Context, is *v1alpha1.MachineSet, newScale int32, deployment *v1alpha1.MachineDeployment, scalingOperation string) (bool, *v1alpha1.MachineSet, error) {
	isCopy := is.DeepCopy()

	sizeNeedsUpdate := (isCopy.Spec.Replicas) != newScale
	// TODO: Do not mutate the machine set here, instead simply compare the annotation and if they mismatch
	// call SetReplicasAnnotations inside the following if clause. Then we can also move the deep-copy from
	// above inside the if too.
	annotationsNeedUpdate := SetReplicasAnnotations(isCopy, (deployment.Spec.Replicas), (deployment.Spec.Replicas)+MaxSurge(*deployment))

	scaled := false
	var err error
	if sizeNeedsUpdate || annotationsNeedUpdate {
		isCopy.Spec.Replicas = newScale
		is, err = dc.controlMachineClient.MachineSets(isCopy.Namespace).Update(ctx, isCopy, metav1.UpdateOptions{})
		if err == nil && sizeNeedsUpdate {
			scaled = true
			dc.recorder.Eventf(deployment, v1.EventTypeNormal, "ScalingMachineSet", "Scaled %s machine set %s to %d", scalingOperation, is.Name, newScale)
		}
	}
	return scaled, is, err
}

// cleanupDeployment is responsible for cleaning up a deployment ie. retains all but the latest N old machine sets
// where N=d.Spec.RevisionHistoryLimit. Old machine sets are older versions of the machinetemplate of a deployment kept
// around by default 1) for historical reasons and 2) for the ability to rollback a deployment.
func (dc *controller) cleanupMachineDeployment(ctx context.Context, oldISs []*v1alpha1.MachineSet, deployment *v1alpha1.MachineDeployment) error {
	if deployment.Spec.RevisionHistoryLimit == nil {
		return nil
	}

	// Avoid deleting machine set with deletion timestamp set
	aliveFilter := func(is *v1alpha1.MachineSet) bool {
		return is != nil && is.ObjectMeta.DeletionTimestamp == nil
	}
	cleanableISes := FilterMachineSets(oldISs, aliveFilter)

	diff := int32(len(cleanableISes)) - *deployment.Spec.RevisionHistoryLimit // #nosec G115 (CWE-190) -- number will never reach MaxInt32, and len() cannot be negative
	if diff <= 0 {
		return nil
	}

	sort.Sort(MachineSetsByCreationTimestamp(cleanableISes))
	klog.V(4).Infof("Looking to cleanup old machine sets for deployment %q", deployment.Name)

	for i := int32(0); i < diff; i++ {
		is := cleanableISes[i]
		// Avoid delete machine set with non-zero replica counts
		if is.Status.Replicas != 0 || (is.Spec.Replicas) != 0 || is.Generation > is.Status.ObservedGeneration || is.DeletionTimestamp != nil {
			continue
		}
		klog.V(3).Infof("Trying to cleanup machine set %q for deployment %q", is.Name, deployment.Name)
		if err := dc.controlMachineClient.MachineSets(is.Namespace).Delete(ctx, is.Name, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
			// Return error instead of aggregating and continuing DELETEs on the theory
			// that we may be overloading the api server.
			return err
		}
	}

	return nil
}

// syncDeploymentStatus checks if the status is up-to-date and sync it if necessary
func (dc *controller) syncMachineDeploymentStatus(ctx context.Context, allISs []*v1alpha1.MachineSet, newIS *v1alpha1.MachineSet, d *v1alpha1.MachineDeployment) error {
	newStatus := calculateDeploymentStatus(allISs, newIS, d)

	if reflect.DeepEqual(d.Status, newStatus) {
		return nil
	}

	newDeployment := d
	newDeployment.Status = newStatus
	_, err := dc.controlMachineClient.MachineDeployments(newDeployment.Namespace).UpdateStatus(ctx, newDeployment, metav1.UpdateOptions{})
	return err
}

// calculateStatus calculates the latest status for the provided deployment by looking into the provided machine sets.
func calculateDeploymentStatus(allISs []*v1alpha1.MachineSet, newIS *v1alpha1.MachineSet, deployment *v1alpha1.MachineDeployment) v1alpha1.MachineDeploymentStatus {
	availableReplicas := GetAvailableReplicaCountForMachineSets(allISs)
	totalReplicas := GetReplicaCountForMachineSets(allISs)
	unavailableReplicas := totalReplicas - availableReplicas
	// If unavailableReplicas is negative, then that means the Deployment has more available replicas running than
	// desired, e.g. whenever it scales down. In such a case we should simply default unavailableReplicas to zero.
	if unavailableReplicas < 0 {
		unavailableReplicas = 0
	}

	status := v1alpha1.MachineDeploymentStatus{
		// TODO: Ensure that if we start retrying status updates, we won't pick up a new Generation value.
		ObservedGeneration:  deployment.Generation,
		Replicas:            GetActualReplicaCountForMachineSets(allISs),
		UpdatedReplicas:     GetActualReplicaCountForMachineSets([]*v1alpha1.MachineSet{newIS}),
		ReadyReplicas:       GetReadyReplicaCountForMachineSets(allISs),
		AvailableReplicas:   availableReplicas,
		UnavailableReplicas: unavailableReplicas,
		CollisionCount:      deployment.Status.CollisionCount,
	}
	status.FailedMachines = []*v1alpha1.MachineSummary{}

	for _, is := range allISs {
		if is != nil && is.Status.FailedMachines != nil {
			for idx := range *is.Status.FailedMachines {
				// Memory pointed by FailedMachines's pointer fields should never be altered using them
				// as they point to the machineset object's fields, and only machineset controller should alter them
				status.FailedMachines = append(status.FailedMachines, &(*is.Status.FailedMachines)[idx])
			}
		}
	}

	// Copy conditions one by one so we won't mutate the original object.
	status.Conditions = append(status.Conditions, deployment.Status.Conditions...)

	if availableReplicas >= (deployment.Spec.Replicas)-MaxUnavailable(*deployment) {
		minAvailability := NewMachineDeploymentCondition(v1alpha1.MachineDeploymentAvailable, v1alpha1.ConditionTrue, MinimumReplicasAvailable, "Deployment has minimum availability.")
		SetMachineDeploymentCondition(&status, *minAvailability)
	} else {
		noMinAvailability := NewMachineDeploymentCondition(v1alpha1.MachineDeploymentAvailable, v1alpha1.ConditionFalse, MinimumReplicasUnavailable, "Deployment does not have minimum availability.")
		SetMachineDeploymentCondition(&status, *noMinAvailability)
	}

	return status
}

// isScalingEvent checks whether the provided deployment has been updated with a scaling event
// by looking at the desired-replicas annotation in the active machine sets of the deployment, and returns if there was scale-out or not.
// However, when there are no active machine sets, but the replica count in the machine deployment's spec > 0, it is recognized as a scale-out event.
//
// rsList should come from getReplicaSetsForDeployment(d).
// machineMap should come from getmachineMapForDeployment(d, rsList).
func (dc *controller) isScalingEvent(ctx context.Context, d *v1alpha1.MachineDeployment, isList []*v1alpha1.MachineSet, machineMap map[types.UID]*v1alpha1.MachineList) (bool, error) {
	newIS, oldISs, err := dc.getAllMachineSetsAndSyncRevision(ctx, d, isList, machineMap, false)
	if err != nil {
		return false, err
	}
	if newIS == nil {
		return false, nil
	}
	allISs := append(oldISs, newIS)
	activeMachineSets := FilterActiveMachineSets(allISs)
	//if this is a scale from zero scenario, return true
	if len(activeMachineSets) == 0 && d.Spec.Replicas > 0 {
		return true, nil
	}
	for _, is := range activeMachineSets {
		desired, ok := GetDesiredReplicasAnnotation(is)
		if !ok {
			continue
		}
		if desired != (d.Spec.Replicas) {
			klog.V(2).Infof("Desired replicas annotation value: %d on machineSet %s, Spec Desired Replicas value: %d on corresponding machineDeployment, so scaling has happened.", desired, is.Name, d.Spec.Replicas)
			return true, nil
		}
	}
	return false, nil
}
