package pgbouncerservice

/*
Copyright 2018 - 2020 Crunchy Data Solutions, Inc.
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

import (
	"fmt"
	"strings"

	crv1 "github.com/crunchydata/postgres-operator/apis/crunchydata.com/v1"
	"github.com/crunchydata/postgres-operator/apiserver"
	msgs "github.com/crunchydata/postgres-operator/apiservermsgs"
	"github.com/crunchydata/postgres-operator/config"
	"github.com/crunchydata/postgres-operator/kubeapi"
	clusteroperator "github.com/crunchydata/postgres-operator/operator/cluster"
	"github.com/crunchydata/postgres-operator/util"

	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const pgBouncerServiceSuffix = "-pgbouncer"

// CreatePgbouncer ...
// pgo create pgbouncer mycluster
// pgo create pgbouncer --selector=name=mycluster
func CreatePgbouncer(request *msgs.CreatePgbouncerRequest, ns, pgouser string) msgs.CreatePgbouncerResponse {
	var err error
	resp := msgs.CreatePgbouncerResponse{}
	resp.Status.Code = msgs.Ok
	resp.Status.Msg = ""
	resp.Results = make([]string, 0)

	// validate the CPU/Memory request parameters, if they are passed in
	if err := apiserver.ValidateQuantity(request.CPURequest); err != nil {
		resp.Status.Code = msgs.Error
		resp.Status.Msg = fmt.Sprintf(apiserver.ErrMessageCPURequest,
			request.CPURequest, err.Error())
		return resp
	}

	if err := apiserver.ValidateQuantity(request.MemoryRequest); err != nil {
		resp.Status.Code = msgs.Error
		resp.Status.Msg = fmt.Sprintf(apiserver.ErrMessageMemoryRequest,
			request.MemoryRequest, err.Error())
		return resp
	}

	// validate the number of replicas being requested
	if request.Replicas < 0 {
		resp.Status.Code = msgs.Error
		resp.Status.Msg = fmt.Sprintf(apiserver.ErrMessageReplicas, 1)
		return resp
	}

	log.Debugf("createPgbouncer selector is [%s]", request.Selector)

	// try to get the list of clusters. if there is an error, put it into the
	// status and return
	clusterList, err := getClusterList(request.Namespace, request.Args, request.Selector)

	if err != nil {
		resp.Status.Code = msgs.Error
		resp.Status.Msg = err.Error()
		return resp
	}

	for _, cluster := range clusterList.Items {
		log.Debugf("adding pgbouncer to cluster [%s]", cluster.Name)

		resources := v1.ResourceList{}

		// Set the value that enables the pgBouncer, which is the replicas
		// Set the default value, and if there is a custom number of replicas
		// provided, set it to that
		cluster.Spec.PgBouncer.Replicas = config.DefaultPgBouncerReplicas

		if request.Replicas > 0 {
			cluster.Spec.PgBouncer.Replicas = request.Replicas
		}

		// if the request has overriding CPURequest and/or MemoryRequest parameters,
		// these will take precedence over the defaults
		if request.CPURequest != "" {
			// as this was already validated, we can ignore the error
			quantity, _ := resource.ParseQuantity(request.CPURequest)
			resources[v1.ResourceCPU] = quantity
		}

		if request.MemoryRequest != "" {
			// as this was already validated, we can ignore the error
			quantity, _ := resource.ParseQuantity(request.MemoryRequest)
			resources[v1.ResourceMemory] = quantity
		} else {
			resources[v1.ResourceMemory] = apiserver.Pgo.Cluster.DefaultPgBouncerResourceMemory
		}

		cluster.Spec.PgBouncer.Resources = resources

		// update the cluster CRD with these udpates. If there is an error
		if err := kubeapi.Updatepgcluster(apiserver.RESTClient, &cluster, cluster.Name, request.Namespace); err != nil {
			log.Error(err)
			resp.Results = append(resp.Results, err.Error())
			continue
		}

		resp.Results = append(resp.Results, fmt.Sprintf("%s pgbouncer added", cluster.Name))
	}

	return resp
}

// DeletePgbouncer ...
// pgo delete pgbouncer mycluster
// pgo delete pgbouncer --selector=name=mycluster
func DeletePgbouncer(request *msgs.DeletePgbouncerRequest, ns string) msgs.DeletePgbouncerResponse {
	var err error
	resp := msgs.DeletePgbouncerResponse{}
	resp.Status.Code = msgs.Ok
	resp.Status.Msg = ""
	resp.Results = make([]string, 0)

	log.Debugf("deletePgbouncer selector is [%s]", request.Selector)

	// try to get the list of clusters. if there is an error, put it into the
	// status and return
	clusterList, err := getClusterList(request.Namespace, request.Args, request.Selector)

	if err != nil {
		resp.Status.Code = msgs.Error
		resp.Status.Msg = err.Error()
		return resp
	}

	// Return an error if any clusters identified to have pgbouncer fully deleted (as specified
	// using the uninstall parameter) have standby mode enabled and the 'uninstall' option selected.
	// This because while in standby mode the cluster is read-only, preventing the execution of the
	// SQL required to remove pgBouncer.
	if hasStandby, standbyClusters := apiserver.PGClusterListHasStandby(clusterList); hasStandby &&
		request.Uninstall {

		resp.Status.Code = msgs.Error
		resp.Status.Msg = fmt.Sprintf("Request rejected, unable to delete pgbouncer using the "+
			"'uninstall' parameter for clusters %s: %s.", strings.Join(standbyClusters, ","),
			apiserver.ErrStandbyNotAllowed.Error())
		return resp
	}

	for _, cluster := range clusterList.Items {
		log.Debugf("deleting pgbouncer from cluster [%s]", cluster.Name)

		// check to see if the uninstall flag was set. If it was, apply the update
		// inline
		if request.Uninstall {
			if err := clusteroperator.UninstallPgBouncer(apiserver.Clientset, apiserver.RESTConfig, &cluster); err != nil {
				log.Error(err)
				resp.Status.Code = msgs.Error
				resp.Results = append(resp.Results, err.Error())
				return resp
			}
		}

		// Disable the pgBouncer Deploymnet, whcih means setting Replicas to 0
		cluster.Spec.PgBouncer.Replicas = 0

		// update the cluster CRD with these udpates. If there is an error
		if err := kubeapi.Updatepgcluster(apiserver.RESTClient, &cluster, cluster.Name, request.Namespace); err != nil {
			log.Error(err)
			resp.Status.Code = msgs.Error
			resp.Results = append(resp.Results, err.Error())
			return resp
		}

		// follow the legacy format for returning this information
		result := fmt.Sprintf("%s pgbouncer deleted", cluster.Name)
		resp.Results = append(resp.Results, result)
	}

	return resp

}

// ShowPgBouncer gets information about a PostgreSQL cluster's pgBouncer
// deployment
//
// pgo show pgbouncer
// pgo show pgbouncer --selector
func ShowPgBouncer(request *msgs.ShowPgBouncerRequest, namespace string) msgs.ShowPgBouncerResponse {
	// set up a dummy response
	response := msgs.ShowPgBouncerResponse{
		Results: []msgs.ShowPgBouncerDetail{},
		Status: msgs.Status{
			Code: msgs.Ok,
			Msg:  "",
		},
	}

	log.Debugf("show pgbouncer called, cluster [%v], selector [%s]", request.ClusterNames, request.Selector)

	// try to get the list of clusters. if there is an error, put it into the
	// status and return
	clusterList, err := getClusterList(request.Namespace, request.ClusterNames, request.Selector)

	if err != nil {
		response.Status.Code = msgs.Error
		response.Status.Msg = err.Error()
		return response
	}

	// iterate through the list of clusters to get the relevant pgBouncer
	// information about them
	for _, cluster := range clusterList.Items {
		result := msgs.ShowPgBouncerDetail{
			ClusterName:  cluster.Spec.Name,
			HasPgBouncer: true,
		}
		// first, check if the cluster has pgBouncer enabled
		if !cluster.Spec.PgBouncer.Enabled() {
			result.HasPgBouncer = false
			response.Results = append(response.Results, result)
			continue
		}

		// only set the pgBouncer user if we know this is a pgBouncer enabled
		// cluster...even though, yes, this is a constant
		result.Username = crv1.PGUserPgBouncer

		// set the pgBouncer service information on this record
		setPgBouncerServiceDetail(cluster, &result)

		// get the user information about the pgBouncer deployment
		setPgBouncerPasswordDetail(cluster, &result)

		// append the result to the list
		response.Results = append(response.Results, result)
	}

	return response
}

// UpdatePgBouncer updates a cluster's pgBouncer deployment based on the
// parameters passed in. This includes:
//
// - password rotation
// - updating CPU/memory resources
func UpdatePgBouncer(request *msgs.UpdatePgBouncerRequest, namespace, pgouser string) msgs.UpdatePgBouncerResponse {
	// set up a dummy response
	response := msgs.UpdatePgBouncerResponse{
		// Results: []msgs.ShowPgBouncerDetail{},
		Status: msgs.Status{
			Code: msgs.Ok,
			Msg:  "",
		},
	}

	// validate the CPU/Memory request parameters, if they are passed in
	if err := apiserver.ValidateQuantity(request.CPURequest); err != nil {
		response.Status.Code = msgs.Error
		response.Status.Msg = fmt.Sprintf(apiserver.ErrMessageCPURequest,
			request.CPURequest, err.Error())
		return response
	}

	if err := apiserver.ValidateQuantity(request.MemoryRequest); err != nil {
		response.Status.Code = msgs.Error
		response.Status.Msg = fmt.Sprintf(apiserver.ErrMessageMemoryRequest,
			request.MemoryRequest, err.Error())
		return response
	}

	// validate the number of replicas being requested
	if request.Replicas < 0 {
		response.Status.Code = msgs.Error
		response.Status.Msg = fmt.Sprintf(apiserver.ErrMessageReplicas, 1)
		return response
	}

	log.Debugf("update pgbouncer called, cluster [%v], selector [%s]", request.ClusterNames, request.Selector)

	// try to get the list of clusters. if there is an error, put it into the
	// status and return
	clusterList, err := getClusterList(request.Namespace, request.ClusterNames, request.Selector)

	if err != nil {
		response.Status.Code = msgs.Error
		response.Status.Msg = err.Error()
		return response
	}

	// Return an error if any clusters selected to have pgbouncer updated have standby mode enabled.
	// This is because while in standby mode the cluster is read-only, preventing the execution of the
	// SQL required to update pgbouncer.
	if hasStandby, standbyClusters := apiserver.PGClusterListHasStandby(clusterList); hasStandby {

		response.Status.Code = msgs.Error
		response.Status.Msg = fmt.Sprintf("Request rejected, unable to update pgbouncer for "+
			"clusters %s: %s.", strings.Join(standbyClusters, ","),
			apiserver.ErrStandbyNotAllowed.Error())
		return response
	}

	// iterate through the list of clusters to get the relevant pgBouncer
	// information about them
	for _, cluster := range clusterList.Items {
		result := msgs.UpdatePgBouncerDetail{
			ClusterName:  cluster.Spec.Name,
			HasPgBouncer: true,
		}

		// first, check if the cluster has pgBouncer enabled
		if !cluster.Spec.PgBouncer.Enabled() {
			result.HasPgBouncer = false
			response.Results = append(response.Results, result)
			continue
		}

		// if we are rotating the password, perform the request inline
		if request.RotatePassword {
			if err := clusteroperator.RotatePgBouncerPassword(apiserver.Clientset, apiserver.RESTClient, apiserver.RESTConfig, &cluster); err != nil {
				log.Error(err)
				result.Error = true
				result.ErrorMessage = err.Error()
				response.Results = append(response.Results, result)
			}
		}

		// if the request has overriding CPURequest and/or MemoryRequest parameters,
		// add them to the cluster's pgbouncer resource list
		resources := v1.ResourceList{}

		if request.CPURequest != "" {
			// as this was already validated, we can ignore the error
			quantity, _ := resource.ParseQuantity(request.CPURequest)
			resources[v1.ResourceCPU] = quantity
		}

		if request.MemoryRequest != "" {
			// as this was already validated, we can ignore the error
			quantity, _ := resource.ParseQuantity(request.MemoryRequest)
			resources[v1.ResourceMemory] = quantity
		}

		// update the resources, if there are any changes
		if len(resources) > 0 {
			if cluster.Spec.PgBouncer.Resources == nil {
				cluster.Spec.PgBouncer.Resources = resources
			} else {
				for resource, quantity := range resources {
					cluster.Spec.PgBouncer.Resources[resource] = quantity
				}
			}
		}

		// apply the replica count number if there is a change, i.e. replicas is not
		// 0
		if request.Replicas > 0 {
			cluster.Spec.PgBouncer.Replicas = request.Replicas
		}

		if err := kubeapi.Updatepgcluster(apiserver.RESTClient, &cluster, cluster.Name, cluster.Namespace); err != nil {
			log.Error(err)
			result.Error = true
			result.ErrorMessage = err.Error()
			response.Results = append(response.Results, result)
			continue
		}

		// append the result to the list
		response.Results = append(response.Results, result)
	}

	return response
}

// getClusterList tries to return a list of clusters based on either having an
// argument list of cluster names, or a Kubernetes selector
func getClusterList(namespace string, clusterNames []string, selector string) (crv1.PgclusterList, error) {
	clusterList := crv1.PgclusterList{}

	// see if there are any values in the cluster name list or in the selector
	// if nothing exists, return an error
	if len(clusterNames) == 0 && selector == "" {
		err := fmt.Errorf("either a list of cluster names or a selector needs to be supplied for this comment")
		return clusterList, err
	}

	// try to build the cluster list based on either the selector or the list
	// of arguments...or both. First, start with the selector
	if selector != "" {
		err := kubeapi.GetpgclustersBySelector(apiserver.RESTClient, &clusterList,
			selector, namespace)

		// if there is an error, return here with an empty cluster list
		if err != nil {
			return crv1.PgclusterList{}, err
		}
	}

	// now try to get clusters based specific cluster names
	for _, clusterName := range clusterNames {
		cluster := crv1.Pgcluster{}

		found, err := kubeapi.Getpgcluster(apiserver.RESTClient, &cluster,
			clusterName, namespace)

		// if there is an error, capture it here and return here with an empty list
		if !found || err != nil {
			return crv1.PgclusterList{}, err
		}

		// if successful, append to the cluster list
		clusterList.Items = append(clusterList.Items, cluster)
	}

	log.Debugf("clusters founds: [%d]", len(clusterList.Items))

	// if after all this, there are no clusters found, return an error
	if len(clusterList.Items) == 0 {
		err := fmt.Errorf("no clusters found")
		return clusterList, err
	}

	// all set! return the cluster list with error
	return clusterList, nil
}

// setPgBouncerPasswordDetail applies the password that is used by the pgbouncer
// service account
func setPgBouncerPasswordDetail(cluster crv1.Pgcluster, result *msgs.ShowPgBouncerDetail) {
	pgBouncerSecretName := util.GeneratePgBouncerSecretName(cluster.Spec.Name)

	// attempt to get the secret, but only get the password
	password, err := util.GetPasswordFromSecret(apiserver.Clientset,
		cluster.Spec.Namespace, pgBouncerSecretName)

	if err != nil {
		log.Warn(err)
	}

	// and set the password. Easy!
	result.Password = password
}

// setPgBouncerServiceDetail applies the information about the pgBouncer service
// to the result for the pgBouncer show
func setPgBouncerServiceDetail(cluster crv1.Pgcluster, result *msgs.ShowPgBouncerDetail) {
	// get the service information about the pgBouncer deployment
	selector := fmt.Sprintf("%s=%s", config.LABEL_PG_CLUSTER, cluster.Spec.Name)

	// have to go through a bunch of services because "current design"
	services, err := kubeapi.GetServices(apiserver.Clientset, selector, cluster.Spec.Namespace)

	// if there is an error, return without making any adjustments
	if err != nil {
		log.Warn(err)
		return
	}

	log.Debugf("cluster [%s] has [%d] services", cluster.Spec.Name, len(services.Items))

	// adding the service information was borrowed from the ShowCluster
	// resource
	for _, service := range services.Items {
		// if this service is not for pgBouncer, then skip
		if !strings.HasSuffix(service.Name, pgBouncerServiceSuffix) {
			continue
		}

		// this is the pgBouncer service!
		result.ServiceClusterIP = service.Spec.ClusterIP
		result.ServiceName = service.Name

		// try to get the exterinal IP based on the formula used in show cluster
		if len(service.Spec.ExternalIPs) > 0 {
			result.ServiceExternalIP = service.Spec.ExternalIPs[0]
		}

		if len(service.Status.LoadBalancer.Ingress) > 0 {
			result.ServiceExternalIP = service.Status.LoadBalancer.Ingress[0].IP
		}
	}
}
