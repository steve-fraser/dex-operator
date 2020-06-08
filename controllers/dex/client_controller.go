/*
Copyright 2020 Betsson Group.

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

package controllers

import (
	"context"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	dexv1 "github.com/BetssonGroup/dex-operator/apis/dex/v1"
	dexapi "github.com/BetssonGroup/dex-operator/pkg/dex"
)

var (
	clientsCreated = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "client_created_total",
			Help: "Number of clients created",
		},
	)
	clientFailures = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "client_failures_total",
			Help: "Number of failed clients",
		},
	)
)

func init() {
	metrics.Registry.MustRegister(clientsCreated, clientFailures)
}

// ClientReconciler reconciles a Client object
type ClientReconciler struct {
	client.Client
	Log       logr.Logger
	Scheme    *runtime.Scheme
	DexClient *dexapi.APIClient
	Recorder  record.EventRecorder
}

// +kubebuilder:rbac:groups=dex.betssongroup.com,resources=clients,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dex.betssongroup.com,resources=clients/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile reconciles oidc clients in dex
func (r *ClientReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("client", req.NamespacedName)

	// Get an oauth2 Client
	dexv1Client := &dexv1.Client{}
	if err := r.Get(ctx, req.NamespacedName, dexv1Client); err != nil {
		// we'll ignore not-found errors, since they can't be fixed by an immediate
		// requeue (we'll need to wait for a new notification), and we can get them
		// on deleted requests.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// deletion finalizer
	dexFinalizer := "client.dex.finalizers.betssongroup.com"

	// Delete
	// examine DeletionTimestamp to determine if object is under deletion
	if dexv1Client.ObjectMeta.DeletionTimestamp.IsZero() {
		if !containsString(dexv1Client.ObjectMeta.Finalizers, dexFinalizer) {
			// append our finalizer
			log.Info("Adding finalizer")
			dexv1Client.ObjectMeta.Finalizers = append(dexv1Client.ObjectMeta.Finalizers, dexFinalizer)
			if err := r.Update(ctx, dexv1Client); err != nil {
				return ctrl.Result{}, err
			}
		}
	} else {
		// The object is being deleted
		if containsString(dexv1Client.ObjectMeta.Finalizers, dexFinalizer) {
			r.Recorder.Eventf(dexv1Client, "Normal", "ClientDeletion", "client %s", dexv1Client.Name)
			// our finalizer is present, so lets handle any external dependency
			if dexv1Client.Status.State != dexv1.PhaseFailed { // It's a failed client, just delete it.
				dexv1Client.Status.State = dexv1.PhaseDeleting
				if err := r.DexClient.DeleteClient(ctx, dexv1Client.Name); err != nil {
					dexv1Client.Status.Message = err.Error()
					if err := r.Update(ctx, dexv1Client); err != nil {
						r.Recorder.Eventf(dexv1Client, "Error", "ClientDeletion", "client %s: %s", dexv1Client.Name, err.Error())
						return ctrl.Result{}, err
					}
					return ctrl.Result{}, err
				}
			}
			// remove our finalizer from the list and update it.
			dexv1Client.ObjectMeta.Finalizers = removeString(dexv1Client.ObjectMeta.Finalizers, dexFinalizer)
			if err := r.Update(ctx, dexv1Client); err != nil {
				return ctrl.Result{}, err
			}
		}
		// Stop reconciliation as the item is being deleted
		return ctrl.Result{}, nil
	}

	// if status is not set, set it to CREATING
	if dexv1Client.Status.State == "" || dexv1Client.Status.State == dexv1.PhaseCreating {
		dexv1Client.Status.State = dexv1.PhaseCreating
	}
	// Now let's make the main case distinction: implementing
	// the state diagram CREATING -> ACTIVE or CREATING -> FAILED
	switch dexv1Client.Status.State {
	case dexv1.PhaseCreating:
		log.Info("Creating dex client", "name", dexv1Client.Name)
		// Implement dex auth client creation here
		res, err := r.DexClient.CreateClient(
			ctx,
			dexv1Client.Spec.RedirectURIs,
			dexv1Client.Spec.TrustedPeers,
			dexv1Client.Spec.Public,
			dexv1Client.Spec.Name,
			dexv1Client.Name,
			dexv1Client.Spec.LogoURL,
			dexv1Client.Spec.Secret,
		)
		if err != nil {
			log.Error(err, "Client create failed", "client", dexv1Client.Name)
			dexv1Client.Status.State = dexv1.PhaseFailed
			dexv1Client.Status.Message = err.Error()
			r.Recorder.Eventf(dexv1Client, "Error", "ClientCreation", "client %s: %s", dexv1Client.Name, err.Error())
			clientFailures.Inc()
		} else {
			dexv1Client.Status.State = dexv1.PhaseActive
			log.Info("Client created", "client ID", res.GetId())
			r.Recorder.Eventf(dexv1Client, "Normal", "ClientCreation", "client %s", dexv1Client.Name)
			clientsCreated.Inc()
		}
	case dexv1.PhaseActive:
		// If the client is active but in the reconcile loop it's being updated.
		log.Info("Client update", "client ID", dexv1Client.Name)
		err := r.DexClient.UpdateClient(
			ctx,
			dexv1Client.Name,
			dexv1Client.Spec.RedirectURIs,
			dexv1Client.Spec.TrustedPeers,
			dexv1Client.Spec.Public,
			dexv1Client.Spec.Name,
			dexv1Client.Spec.LogoURL,
		)
		if err != nil {
			log.Error(err, "Client update failed", "client", dexv1Client.Name)
			dexv1Client.Status.State = dexv1.PhaseActiveDegraded
			dexv1Client.Status.Message = err.Error()
		} else {
			log.Info("Client updated", "client ID", dexv1Client.Name)
			r.Recorder.Eventf(dexv1Client, "Normal", "ClientUpdate", "client %s", dexv1Client.Name)
		}
	case dexv1.PhaseFailed:
		log.Info("Client failed")
	default:
		// Should never reach here
		log.Info("Got an invalid state", "state", dexv1Client.Status.State)
		return ctrl.Result{}, nil
	}
	// Update the object and return
	err := r.Update(ctx, dexv1Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the mananager
func (r *ClientReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dexv1.Client{}).
		Complete(r)
}

// Helper functions to check and remove string from a slice of strings.
func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

func removeString(slice []string, s string) (result []string) {
	for _, item := range slice {
		if item == s {
			continue
		}
		result = append(result, item)
	}
	return
}
