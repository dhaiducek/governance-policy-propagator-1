// Copyright (c) 2021 Red Hat, Inc.
// Copyright Contributors to the Open Cluster Management project

package propagator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	retry "github.com/avast/retry-go/v3"
	"github.com/go-logr/logr"
	clusterv1alpha1 "github.com/open-cluster-management/api/cluster/v1alpha1"
	templates "github.com/open-cluster-management/go-template-utils/pkg/templates"
	policiesv1 "github.com/open-cluster-management/governance-policy-propagator/api/v1"
	"github.com/open-cluster-management/governance-policy-propagator/controllers/common"
	appsv1 "github.com/open-cluster-management/multicloud-operators-placementrule/pkg/apis/apps/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const attemptsDefault = 3
const attemptsEnvName = "CONTROLLER_CONFIG_RETRY_ATTEMPTS"

// The configuration in minutes to requeue after if something failed after several
// retries.
const requeueErrorDelayEnvName = "CONTROLLER_CONFIG_REQUEUE_ERROR_DELAY"
const requeueErrorDelayDefault = 5

var attempts int
var requeueErrorDelay int
var kubeConfig *rest.Config
var kubeClient *kubernetes.Interface
var templateCfg templates.Config

func Initialize(kubeconfig *rest.Config, kubeclient *kubernetes.Interface) {
	kubeConfig = kubeconfig
	kubeClient = kubeclient
	// Adding four spaces to the indentation makes the usage of `indent N` be from the logical
	// starting point of the resource object wrapped in the ConfigurationPolicy.
	templateCfg = templates.Config{
		AdditionalIndentation: 8,
		DisabledFunctions:     []string{"fromSecret"},
		StartDelim:            "{{hub", StopDelim: "hub}}",
	}

	attempts = getEnvVarPosInt(attemptsEnvName, attemptsDefault)
	requeueErrorDelay = getEnvVarPosInt(requeueErrorDelayEnvName, requeueErrorDelayDefault)
}

func getEnvVarPosInt(name string, defaultValue int) int {
	var envValue = os.Getenv(name)
	if envValue == "" {
		return defaultValue
	}

	envInt, err := strconv.Atoi(envValue)
	if err == nil && envInt > 0 {
		return envInt
	}

	log.Info(
		fmt.Sprintf(
			"The %s environment variable is invalid. Using default.", name,
		),
	)
	return defaultValue
}

// The options to call retry.Do with
func getRetryOptions(logger logr.Logger, retryMsg string) []retry.Option {
	return []retry.Option{
		retry.Attempts(uint(attempts)),
		retry.Delay(2 * time.Second),
		retry.MaxDelay(10 * time.Second),
		retry.OnRetry(func(n uint, err error) { logger.Info(retryMsg) }),
		retry.LastErrorOnly(true),
	}
}

// cleanUpPolicy will delete all replicated policies associated with provided policy.
func (r *PolicyReconciler) cleanUpPolicy(instance *policiesv1.Policy) error {
	reqLogger := log.WithValues("Policy-Namespace", instance.GetNamespace(), "Policy-Name", instance.GetName())
	successful := true
	replicatedPlcList := &policiesv1.PolicyList{}

	err := r.List(
		context.TODO(), replicatedPlcList, client.MatchingLabels(common.LabelsForRootPolicy(instance)),
	)
	if err != nil {
		reqLogger.Error(err, "Failed to list the replicated policies...")
		return err
	}

	for _, plc := range replicatedPlcList.Items {
		// #nosec G601 -- no memory addresses are stored in collections
		err := r.Delete(context.TODO(), &plc)
		if err != nil && !k8serrors.IsNotFound(err) {
			reqLogger.Error(err, "Failed to delete replicated policy...", "Namespace", plc.GetNamespace(),
				"Name", plc.GetName())
			successful = false
		}
	}

	if !successful {
		return errors.New("failed to delete one or more replicated policies")
	}

	return nil
}

// handleDecisions will get all the placement decisions based on the input policy and placement
// binding list and propagate the policy. It returns the following:
// * placements - a slice of all the placement decisions discovered
// * allDecisions - a set of all the placement decisions encountered in the format of
//   <namespace>/<name>
// * failedClusters - a set of all the clusters that encountered an error during propagation in the
//   format of <namespace>/<name>
// * allFailed - a bool that determines if all clusters encountered an error during propagation
func (r *PolicyReconciler) handleDecisions(
	instance *policiesv1.Policy, pbList *policiesv1.PlacementBindingList,
) (
	placements []*policiesv1.Placement, allDecisions map[string]bool, failedClusters map[string]bool, allFailed bool,
) {
	reqLogger := log.WithValues("Policy-Namespace", instance.GetNamespace(), "Policy-Name", instance.GetName())
	allDecisions = map[string]bool{}
	failedClusters = map[string]bool{}

	for _, pb := range pbList.Items {
		subjects := pb.Subjects
		for _, subject := range subjects {
			if !(subject.APIGroup == policiesv1.SchemeGroupVersion.Group &&
				subject.Kind == policiesv1.Kind &&
				subject.Name == instance.GetName()) {

				continue
			}

			var decisions []appsv1.PlacementDecision
			var p *policiesv1.Placement
			err := retry.Do(
				func() error {
					var err error
					decisions, p, err = getPlacementDecisions(r.Client, pb, instance)
					return err
				},
				getRetryOptions(reqLogger, "Retrying to get the placement decisions...")...,
			)

			if err != nil {
				reqLogger.Info("Giving up on getting the placement decisions...")
				allFailed = true
				return
			}

			placements = append(placements, p)
			if instance.Spec.Disabled {
				// Only handle the first match in pb.spec.subjects
				break
			}
			// Only handle replicated policies when the policy is not disabled
			// plr found, checking decision
			for _, decision := range decisions {
				key := fmt.Sprintf("%s/%s", decision.ClusterNamespace, decision.ClusterName)
				allDecisions[key] = true
				// create/update replicated policy for each decision
				err := retry.Do(
					func() error {
						return r.handleDecision(instance, decision)
					},
					getRetryOptions(reqLogger, "Retrying to replicate the policy...")...,
				)

				if err != nil {
					reqLogger.Info(
						fmt.Sprintf(
							"Giving up on replicating the policy %s/%s...",
							decision.ClusterNamespace,
							common.FullNameForPolicy(instance),
						),
					)
					failedClusters[key] = true
				}
			}
			// Only handle the first match in pb.spec.subjects
			break
		}
	}

	return
}

// cleanUpOrphanedRplPolicies compares the status of the input policy against the input placement
// decisions. If the cluster exists in the status but doesn't exist in the input placement
// decisions, then it's considered stale and will be removed.
func (r *PolicyReconciler) cleanUpOrphanedRplPolicies(instance *policiesv1.Policy, allDecisions map[string]bool) error {
	reqLogger := log.WithValues("Policy-Namespace", instance.GetNamespace(), "Policy-Name", instance.GetName())
	successful := true
	for _, cluster := range instance.Status.Status {
		key := fmt.Sprintf("%s/%s", cluster.ClusterNamespace, cluster.ClusterName)
		if allDecisions[key] {
			continue
		}
		// not found in allDecisions, orphan, delete it
		name := common.FullNameForPolicy(instance)
		reqLogger.Info(
			fmt.Sprintf(
				"Deleting orphaned replicated policy %s/%s",
				cluster.ClusterNamespace,
				name,
			),
		)
		err := retry.Do(
			func() error {
				err := r.Delete(context.TODO(), &policiesv1.Policy{
					TypeMeta: metav1.TypeMeta{
						Kind:       policiesv1.Kind,
						APIVersion: policiesv1.SchemeGroupVersion.Group,
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: cluster.ClusterNamespace,
					},
				})

				if err != nil && k8serrors.IsNotFound(err) {
					return nil
				}

				return err
			},
			getRetryOptions(reqLogger, "Retrying to delete the orphaned replicated policy...")...,
		)

		if err != nil {
			successful = false
			reqLogger.Error(
				err,
				fmt.Sprintf(
					"Failed to delete the orphaned replicated policy %s/%s",
					cluster.ClusterNamespace,
					name,
				),
			)
		}
	}

	if !successful {
		return errors.New("one or more orphaned replicated policies failed to be deleted")
	}

	return nil
}

func (r *PolicyReconciler) recordWarning(instance *policiesv1.Policy, msgPrefix string) {
	msg := fmt.Sprintf(
		"%s for the policy %s/%s",
		msgPrefix,
		instance.GetNamespace(),
		instance.GetName(),
	)
	r.Recorder.Event(instance, "Warning", "PolicyPropagation", msg)
}

// handleRootPolicy will properly replicate or clean up when a root policy is updated.
//
// Errors are logged in this method and a summary error is returned. This is because the method
// handles retries and will only return after giving up.
//
// There are several retries within handleRootPolicy. This approach is taken over retrying the whole
// method because it makes the retries more targeted and prevents race conditions, such as a
// placement binding getting updated, from causing inconsistencies.
func (r *PolicyReconciler) handleRootPolicy(instance *policiesv1.Policy) error {
	entry_ts := time.Now()
	defer func() {
		now := time.Now()
		elapsed := now.Sub(entry_ts).Seconds()
		roothandlerMeasure.Observe(elapsed)
	}()

	reqLogger := log.WithValues("Policy-Namespace", instance.GetNamespace(), "Policy-Name", instance.GetName())
	originalInstance := instance.DeepCopy()

	// Clean up the replicated policies if the policy is disabled
	if instance.Spec.Disabled {
		reqLogger.Info("Policy is disabled, doing clean up...")
		err := retry.Do(
			func() error { return r.cleanUpPolicy(instance) },
			getRetryOptions(reqLogger, "Retrying the policy clean up...")...,
		)

		if err != nil {
			reqLogger.Info("Giving up on the policy clean up...")
			r.recordWarning(instance, "One or more replicated policies could not be deleted")
			return err
		}

		r.Recorder.Event(instance, "Normal", "PolicyPropagation",
			fmt.Sprintf("Policy %s/%s was disabled", instance.GetNamespace(), instance.GetName()))
	}

	// Get the placement binding in order to later get the placement decisions
	pbList := &policiesv1.PlacementBindingList{}
	err := retry.Do(
		func() error {
			return r.List(
				context.TODO(), pbList, &client.ListOptions{Namespace: instance.GetNamespace()},
			)
		},
		getRetryOptions(reqLogger, "Retrying to list the placement bindings...")...,
	)

	if err != nil {
		reqLogger.Info("Giving up on listing the placement bindings...")
		r.recordWarning(instance, "Could not list the placement bindings")
		return err
	}

	// allDecisions and failedClusters are sets in the format of <namespace>/<name>
	placements, allDecisions, failedClusters, allFailed := r.handleDecisions(instance, pbList)
	if allFailed {
		reqLogger.Info("Failed to get any placement decisions. Giving up...")
		msg := "Could not get the placement decisions"
		r.recordWarning(instance, msg)
		// Make the error start with a lower case for the linting check
		return errors.New("c" + msg[1:])
	}

	status := []*policiesv1.CompliancePerClusterStatus{}
	if !instance.Spec.Disabled {
		// Get all the replicated policies
		replicatedPlcList := &policiesv1.PolicyList{}
		err := retry.Do(
			func() error {
				return r.List(
					context.TODO(),
					replicatedPlcList,
					client.MatchingLabels(common.LabelsForRootPolicy(instance)),
				)
			},
			getRetryOptions(reqLogger, "Retrying to list the replicated policies...")...,
		)

		if err != nil {
			reqLogger.Info("Giving up on listing the replicated policies...")
			r.recordWarning(instance, "Could not list the replicated policies")
			return err
		}

		// Update the status based on the replicated policies
		for _, rPlc := range replicatedPlcList.Items {
			namespace := rPlc.GetLabels()[common.ClusterNamespaceLabel]
			name := rPlc.GetLabels()[common.ClusterNameLabel]
			key := fmt.Sprintf("%s/%s", namespace, name)

			if failed := failedClusters[key]; failed {
				// Skip the replicated policies that failed to be properly replicated
				// for now. This will be handled later.
				continue
			}

			status = append(status, &policiesv1.CompliancePerClusterStatus{
				ComplianceState:  rPlc.Status.ComplianceState,
				ClusterName:      name,
				ClusterNamespace: namespace,
			})
		}

		// Add cluster statuses for the clusters that did not get their policies properly
		// replicated. This is not done in the previous loop since some replicated polices may not
		// have been created at all.
		for clusterNsName := range failedClusters {
			reqLogger.Info(
				fmt.Sprintf(
					"Setting the policy to noncompliant for %s since the replication failed...",
					clusterNsName,
				),
			)
			// The string split is safe since the namespace and name cannot have slashes in them
			// since they must be DNS compliant names
			clusterNsNameSl := strings.Split(clusterNsName, "/")
			status = append(status, &policiesv1.CompliancePerClusterStatus{
				ComplianceState:  policiesv1.NonCompliant,
				ClusterName:      clusterNsNameSl[1],
				ClusterNamespace: clusterNsNameSl[0],
			})
		}

		sort.Slice(status, func(i, j int) bool {
			return status[i].ClusterName < status[j].ClusterName
		})
	}

	instance.Status.Status = status
	//loop through status and set ComplianceState
	instance.Status.ComplianceState = ""
	isCompliant := true
	for _, cpcs := range status {
		if cpcs.ComplianceState == "NonCompliant" {
			instance.Status.ComplianceState = policiesv1.NonCompliant
			isCompliant = false
			break
		} else if cpcs.ComplianceState == "" {
			isCompliant = false
		}
	}
	// set to compliant only when all status are compliant
	if len(status) > 0 && isCompliant {
		instance.Status.ComplianceState = policiesv1.Compliant
	}
	// looped through all pb, update status.placement
	sort.Slice(placements, func(i, j int) bool {
		return placements[i].PlacementBinding < placements[j].PlacementBinding
	})

	instance.Status.Placement = placements

	err = retry.Do(
		func() error {
			return r.Status().Patch(
				context.TODO(), instance, client.MergeFrom(originalInstance),
			)
		},
		getRetryOptions(reqLogger, "Retrying to update the root policy status...")...,
	)

	if err != nil {
		reqLogger.Error(err, "Giving up on updating the root policy status...")
		r.recordWarning(instance, "Failed to update the policy status")
		return err
	}

	err = r.cleanUpOrphanedRplPolicies(instance, allDecisions)
	if err != nil {
		reqLogger.Error(err, "Giving up on deleting the orphaned replicated policies...")
		r.recordWarning(instance, "Failed to delete orphaned replicated policies")
		return err
	}

	reqLogger.Info("Reconciliation complete.")
	return nil
}

// getApplicationPlacementDecisions return the placement decisions from an application
// lifecycle placementrule
func getApplicationPlacementDecisions(c client.Client, pb policiesv1.PlacementBinding, instance *policiesv1.Policy) ([]appsv1.PlacementDecision, *policiesv1.Placement, error) {
	plr := &appsv1.PlacementRule{}
	err := c.Get(context.TODO(), types.NamespacedName{Namespace: instance.GetNamespace(),
		Name: pb.PlacementRef.Name}, plr)
	// no error when not found
	if err != nil && !k8serrors.IsNotFound(err) {
		log.Error(err, "Failed to get PlacementRule...", "Namespace", instance.GetNamespace(), "Name",
			pb.PlacementRef.Name)
		return nil, nil, err
	}
	// add the PlacementRule to placement, if not found there are no decisions
	placement := &policiesv1.Placement{
		PlacementBinding: pb.GetName(),
		PlacementRule:    plr.GetName(),
		// Decisions:        plr.Status.Decisions,
	}
	return plr.Status.Decisions, placement, nil
}

// getClusterPlacementDecisions return the placement decisions from cluster
// placement decisions
func getClusterPlacementDecisions(c client.Client, pb policiesv1.PlacementBinding, instance *policiesv1.Policy) ([]appsv1.PlacementDecision, *policiesv1.Placement, error) {
	pl := &clusterv1alpha1.Placement{}
	err := c.Get(context.TODO(), types.NamespacedName{Namespace: instance.GetNamespace(),
		Name: pb.PlacementRef.Name}, pl)
	// no error when not found
	if err != nil && !k8serrors.IsNotFound(err) {
		log.Error(err, "Failed to get Placement...", "Namespace", instance.GetNamespace(), "Name",
			pb.PlacementRef.Name)
		return nil, nil, err
	}
	// add current Placement to placement, if not found no decisions will be found
	placement := &policiesv1.Placement{
		PlacementBinding: pb.GetName(),
		Placement:        pl.GetName(),
	}
	list := &clusterv1alpha1.PlacementDecisionList{}
	lopts := &client.ListOptions{Namespace: instance.GetNamespace()}

	opts := client.MatchingLabels{"cluster.open-cluster-management.io/placement": pl.GetName()}
	opts.ApplyToList(lopts)
	err = c.List(context.TODO(), list, lopts)
	// do not error out if not found
	if err != nil && !k8serrors.IsNotFound(err) {
		log.Error(err, "Failed to get PlacementDecision...", "Namespace", instance.GetNamespace(), "Name",
			pb.PlacementRef.Name)
		return nil, nil, err
	}
	var decisions []appsv1.PlacementDecision
	decisions = make([]appsv1.PlacementDecision, 0, len(list.Items))
	for _, item := range list.Items {
		for _, cluster := range item.Status.Decisions {
			decided := &appsv1.PlacementDecision{
				ClusterName:      cluster.ClusterName,
				ClusterNamespace: cluster.ClusterName,
			}
			decisions = append(decisions, *decided)
		}
	}
	return decisions, placement, nil
}

// getPlacementDecisions gets the PlacementDecisions for a PlacementBinding
func getPlacementDecisions(c client.Client, pb policiesv1.PlacementBinding,
	instance *policiesv1.Policy) ([]appsv1.PlacementDecision, *policiesv1.Placement, error) {
	if pb.PlacementRef.APIGroup == appsv1.SchemeGroupVersion.Group &&
		pb.PlacementRef.Kind == "PlacementRule" {
		d, placement, err := getApplicationPlacementDecisions(c, pb, instance)
		if err != nil {
			return nil, nil, err
		}
		return d, placement, nil
	} else if pb.PlacementRef.APIGroup == clusterv1alpha1.SchemeGroupVersion.Group &&
		pb.PlacementRef.Kind == "Placement" {
		d, placement, err := getClusterPlacementDecisions(c, pb, instance)
		if err != nil {
			return nil, nil, err
		}
		return d, placement, nil
	}
	return nil, nil, fmt.Errorf("Placement binding %s/%s reference is not valid", pb.Name, pb.Namespace)
}

func (r *PolicyReconciler) handleDecision(instance *policiesv1.Policy, decision appsv1.PlacementDecision) error {
	reqLogger := log.WithValues("Policy-Namespace", instance.GetNamespace(), "Policy-Name", instance.GetName())
	// retrieve replicated policy in cluster namespace
	replicatedPlc := &policiesv1.Policy{}
	err := r.Get(context.TODO(), types.NamespacedName{Namespace: decision.ClusterNamespace,
		Name: common.FullNameForPolicy(instance)}, replicatedPlc)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			// not replicated, need to create
			replicatedPlc = instance.DeepCopy()
			replicatedPlc.SetName(common.FullNameForPolicy(instance))
			replicatedPlc.SetNamespace(decision.ClusterNamespace)
			replicatedPlc.SetResourceVersion("")
			replicatedPlc.SetFinalizers(nil)
			labels := replicatedPlc.GetLabels()
			if labels == nil {
				labels = map[string]string{}
			}
			labels[common.ClusterNameLabel] = decision.ClusterName
			labels[common.ClusterNamespaceLabel] = decision.ClusterNamespace
			labels[common.RootPolicyLabel] = common.FullNameForPolicy(instance)
			replicatedPlc.SetLabels(labels)

			// Make sure the Owner Reference is cleared
			replicatedPlc.SetOwnerReferences(nil)

			//do a quick check for any template delims in the policy before putting it through
			// template processor
			if policyHasTemplates(instance) {
				// resolve hubTemplate before replicating
				// #nosec G104 -- any errors are logged and recorded in the processTemplates method,
				// but the ignored status will be handled appropriately by the policy controllers on
				// the managed cluster(s).
				r.processTemplates(replicatedPlc, decision, instance)
			}

			reqLogger.Info("Creating replicated policy...", "Namespace", decision.ClusterNamespace,
				"Name", common.FullNameForPolicy(instance))
			err = r.Create(context.TODO(), replicatedPlc)
			if err != nil {
				reqLogger.Error(err, "Failed to create replicated policy...", "Namespace", decision.ClusterNamespace,
					"Name", common.FullNameForPolicy(instance))
				return err
			}
			r.Recorder.Event(instance, "Normal", "PolicyPropagation",
				fmt.Sprintf("Policy %s/%s was propagated to cluster %s/%s", instance.GetNamespace(),
					instance.GetName(), decision.ClusterNamespace, decision.ClusterName))
			//exit after handling the create path, shouldnt be going to through the update path
			return nil
		} else {
			// failed to get replicated object, requeue
			reqLogger.Error(err, "Failed to get replicated policy...", "Namespace", decision.ClusterNamespace,
				"Name", common.FullNameForPolicy(instance))
			return err
		}

	}

	// replicated policy already created, need to compare and patch
	comparePlc := instance
	if policyHasTemplates(instance) {
		//template delimis detected, build a temp holder policy with templates resolved
		//before doing a compare with the replicated policy in the cluster namespaces
		tempResolvedPlc := instance.DeepCopy()
		//resolve hubTemplate before replicating
		// #nosec G104 -- any errors are logged and recorded in the processTemplates method,
		// but the ignored status will be handled appropriately by the policy controllers on
		// the managed cluster(s).
		r.processTemplates(tempResolvedPlc, decision, instance)
		comparePlc = tempResolvedPlc
	}

	if !common.CompareSpecAndAnnotation(comparePlc, replicatedPlc) {
		// update needed
		reqLogger.Info("Root policy and Replicated policy mismatch, updating replicated policy...",
			"Namespace", replicatedPlc.GetNamespace(), "Name", replicatedPlc.GetName())
		replicatedPlc.SetAnnotations(comparePlc.GetAnnotations())
		replicatedPlc.Spec = comparePlc.Spec
		err = r.Update(context.TODO(), replicatedPlc)
		if err != nil {
			reqLogger.Error(err, "Failed to update replicated policy...",
				"Namespace", replicatedPlc.GetNamespace(), "Name", replicatedPlc.GetName())
			return err
		}
		r.Recorder.Event(instance, "Normal", "PolicyPropagation",
			fmt.Sprintf("Policy %s/%s was updated for cluster %s/%s", instance.GetNamespace(),
				instance.GetName(), decision.ClusterNamespace, decision.ClusterName))
	}
	return nil
}

// a helper to quickly check if there are any templates in any of the policy templates
func policyHasTemplates(instance *policiesv1.Policy) bool {
	for _, policyT := range instance.Spec.PolicyTemplates {
		if templates.HasTemplate(policyT.ObjectDefinition.Raw, templateCfg.StartDelim) {
			return true
		}
	}
	return false
}

// iterates through policy definitions  and  processes hub templates
// a special  annotation policy.open-cluster-management.io/trigger-update is used to trigger reprocessing of the
// templates and ensuring that the replicated-policies in cluster is updated only if there is a change.
// this annotation is deleted from the replicated policies and not propagated to the cluster namespaces.

func (r *PolicyReconciler) processTemplates(replicatedPlc *policiesv1.Policy, decision appsv1.PlacementDecision, rootPlc *policiesv1.Policy) error {

	reqLogger := log.WithValues("Policy-Namespace", rootPlc.GetNamespace(), "Policy-Name", rootPlc.GetName(), "Managed-Cluster", decision.ClusterName)
	reqLogger.Info("Processing Templates..")

	annotations := replicatedPlc.GetAnnotations()

	//if disable-templates annotations exists and is true, then exit without processing templates
	if disable, ok := annotations["policy.open-cluster-management.io/disable-templates"]; ok {
		reqLogger.Info("Disable annotations :" + disable)

		if bool_disable, err := strconv.ParseBool(disable); err == nil && bool_disable {
			reqLogger.Info("Detected Annotation to disable templates. Exiting template processing")
			return nil
		}
	}

	//clear the trigger-update annotation, its only for the root policy shouldnt be in  replicated policies
	//as it will cause an unnecessary update to the managed clusters
	if _, ok := annotations["policy.open-cluster-management.io/trigger-update"]; ok {
		delete(annotations, "policy.open-cluster-management.io/trigger-update")
		replicatedPlc.SetAnnotations(annotations)
	}

	templateCfg.LookupNamespace = rootPlc.GetNamespace()
	tmplResolver, err := templates.NewResolver(kubeClient, kubeConfig, templateCfg)
	if err != nil {
		reqLogger.Error(err, "Error instantiating template resolver")
		panic(err)
	}

	//A policy can have multiple policy templates within it, iterate and process each
	for _, policyT := range replicatedPlc.Spec.PolicyTemplates {

		if !templates.HasTemplate(policyT.ObjectDefinition.Raw, templateCfg.StartDelim) {
			continue
		}

		if !isConfigurationPolicy(policyT) {
			// has Templates but not a configuration policy
			err = k8serrors.NewBadRequest("Templates are restricted to only Configuration Policies")
			log.Error(err, "Not a Configuration Policy")

			r.Recorder.Event(rootPlc, "Warning", "PolicyPropagation",
				fmt.Sprintf("Policy %s/%s has templates but it is not a ConfigurationPolicy.", rootPlc.GetName(), rootPlc.GetNamespace()))

			return err
		}

		reqLogger.Info("Found Object Definition with templates")

		templateContext := struct {
			ManagedClusterName string
		}{
			ManagedClusterName: decision.ClusterName,
		}
		resolveddata, tplErr := tmplResolver.ResolveTemplate(policyT.ObjectDefinition.Raw, templateContext)
		if tplErr != nil {
			reqLogger.Error(tplErr, "Failed to resolve templates")

			r.Recorder.Event(rootPlc, "Warning", "PolicyPropagation",
				fmt.Sprintf("Failed to resolve templates for cluster %s/%s: %s", decision.ClusterNamespace, decision.ClusterName, tplErr.Error()))
			//Set an annotation on the policyTemplate(e.g. ConfigurationPolicy)  to the template processing error msg
			//managed clusters will use this when creating a violation
			policyTObjectUnstructured := &unstructured.Unstructured{}
			jsonErr := json.Unmarshal(policyT.ObjectDefinition.Raw, policyTObjectUnstructured)
			if jsonErr != nil {
				//it shouldnt get here but if it did just log a msg
				//its alright, a generic msg will be used on the managedcluster
				reqLogger.Error(jsonErr, fmt.Sprintf("Error unmarshalling to json for Policy %s, Cluster %s.", rootPlc.GetName(), decision.ClusterName))
			} else {
				policyTAnnotations := policyTObjectUnstructured.GetAnnotations()
				if policyTAnnotations == nil {
					policyTAnnotations = make(map[string]string)
				}
				policyTAnnotations["policy.open-cluster-management.io/hub-templates-error"] = tplErr.Error()
				policyTObjectUnstructured.SetAnnotations(policyTAnnotations)
				updatedPolicyT, jsonErr := json.Marshal(policyTObjectUnstructured)
				if jsonErr != nil {
					reqLogger.Error(jsonErr, fmt.Sprintf("Error unmarshalling to json for Policy %s, Cluster %s.", rootPlc.GetName(), decision.ClusterName))
				} else {
					policyT.ObjectDefinition.Raw = updatedPolicyT
				}
			}

			return tplErr
		}

		policyT.ObjectDefinition.Raw = resolveddata

	}
	return nil
}

func isConfigurationPolicy(policyT *policiesv1.PolicyTemplate) bool {
	//check if it is a configuration policy first

	var jsonDef map[string]interface{}
	_ = json.Unmarshal(policyT.ObjectDefinition.Raw, &jsonDef)

	return jsonDef != nil && jsonDef["kind"] == "ConfigurationPolicy"
}
