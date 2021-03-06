package pgtask

/*
Copyright 2017 - 2020 Crunchy Data Solutions, Inc.
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
	"strings"

	crv1 "github.com/crunchydata/postgres-operator/apis/crunchydata.com/v1"
	"github.com/crunchydata/postgres-operator/config"
	"github.com/crunchydata/postgres-operator/kubeapi"
	backrestoperator "github.com/crunchydata/postgres-operator/operator/backrest"
	clusteroperator "github.com/crunchydata/postgres-operator/operator/cluster"
	pgdumpoperator "github.com/crunchydata/postgres-operator/operator/pgdump"
	taskoperator "github.com/crunchydata/postgres-operator/operator/task"
	informers "github.com/crunchydata/postgres-operator/pkg/generated/informers/externalversions/crunchydata.com/v1"
	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// Controller holds connections for the controller
type Controller struct {
	PgtaskConfig      *rest.Config
	PgtaskClient      *rest.RESTClient
	PgtaskClientset   *kubernetes.Clientset
	Queue             workqueue.RateLimitingInterface
	Informer          informers.PgtaskInformer
	PgtaskWorkerCount int
}

// RunWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func (c *Controller) RunWorker(stopCh <-chan struct{}, doneCh chan<- struct{}) {

	go c.waitForShutdown(stopCh)

	for c.processNextItem() {
	}

	log.Debug("pgtask Contoller: worker queue has been shutdown, writing to the done channel")
	doneCh <- struct{}{}
}

// waitForShutdown waits for a message on the stop channel and then shuts down the work queue
func (c *Controller) waitForShutdown(stopCh <-chan struct{}) {
	<-stopCh
	c.Queue.ShutDown()
	log.Debug("pgtask Contoller: recieved stop signal, worker queue told to shutdown")
}

func (c *Controller) processNextItem() bool {
	// Wait until there is a new item in the working queue
	key, quit := c.Queue.Get()
	if quit {
		return false
	}

	log.Debugf("working on %s", key.(string))
	keyParts := strings.Split(key.(string), "/")
	keyNamespace := keyParts[0]
	keyResourceName := keyParts[1]

	log.Debugf("queue got key ns=[%s] resource=[%s]", keyNamespace, keyResourceName)

	// Tell the queue that we are done with processing this key. This unblocks the key for other workers
	// This allows safe parallel processing because two pods with the same key are never processed in
	// parallel.
	defer c.Queue.Done(key)

	tmpTask := crv1.Pgtask{}
	found, err := kubeapi.Getpgtask(c.PgtaskClient, &tmpTask, keyResourceName, keyNamespace)
	if !found {
		log.Errorf("ERROR onAdd getting pgtask : %s", err.Error())
		return false
	}

	//update pgtask
	state := crv1.PgtaskStateProcessed
	message := "Successfully processed Pgtask by controller"
	err = kubeapi.PatchpgtaskStatus(c.PgtaskClient, state, message, &tmpTask, keyNamespace)
	if err != nil {
		log.Errorf("ERROR onAdd updating pgtask status: %s", err.Error())
		return false
	}

	//process the incoming task
	switch tmpTask.Spec.TaskType {
	case crv1.PgtaskMinorUpgrade:
		log.Debug("delete minor upgrade task added")
		clusteroperator.AddUpgrade(c.PgtaskClientset, c.PgtaskClient, &tmpTask, keyNamespace)
	case crv1.PgtaskFailover:
		log.Debug("failover task added")
		if !dupeFailover(c.PgtaskClient, &tmpTask, keyNamespace) {
			clusteroperator.FailoverBase(keyNamespace, c.PgtaskClientset, c.PgtaskClient, &tmpTask, c.PgtaskConfig)
		} else {
			log.Debug("skipping duplicate onAdd failover task %s/%s", keyNamespace, keyResourceName)
		}

	case crv1.PgtaskDeleteData:
		log.Debug("delete data task added")
		if !dupeDeleteData(c.PgtaskClient, &tmpTask, keyNamespace) {
			taskoperator.RemoveData(keyNamespace, c.PgtaskClientset, c.PgtaskClient, &tmpTask)
		} else {
			log.Debug("skipping duplicate onAdd delete data task %s/%s", keyNamespace, keyResourceName)
		}
	case crv1.PgtaskDeleteBackups:
		log.Debug("delete backups task added")
		taskoperator.RemoveBackups(keyNamespace, c.PgtaskClientset, &tmpTask)
	case crv1.PgtaskBackrest:
		log.Debug("backrest task added")
		backrestoperator.Backrest(keyNamespace, c.PgtaskClientset, &tmpTask)
	case crv1.PgtaskBackrestRestore:
		log.Debug("backrest restore task added")
		backrestoperator.Restore(c.PgtaskClient, keyNamespace, c.PgtaskClientset, &tmpTask)

	case crv1.PgtaskpgDump:
		log.Debug("pgDump task added")
		pgdumpoperator.Dump(keyNamespace, c.PgtaskClientset, c.PgtaskClient, &tmpTask)
	case crv1.PgtaskpgRestore:
		log.Debug("pgDump restore task added")
		pgdumpoperator.Restore(keyNamespace, c.PgtaskClientset, c.PgtaskClient, &tmpTask)

	case crv1.PgtaskAutoFailover:
		log.Debugf("autofailover task added %s", keyResourceName)
	case crv1.PgtaskWorkflow:
		log.Debugf("workflow task added [%s] ID [%s]", keyResourceName, tmpTask.Spec.Parameters[crv1.PgtaskWorkflowID])

	case crv1.PgtaskCloneStep1, crv1.PgtaskCloneStep2, crv1.PgtaskCloneStep3:
		log.Debug("clone task added [%s]", keyResourceName)
		clusteroperator.Clone(c.PgtaskClientset, c.PgtaskClient, c.PgtaskConfig, keyNamespace, &tmpTask)

	default:
		log.Debugf("unknown task type on pgtask added [%s]", tmpTask.Spec.TaskType)
	}

	return true

}

// onAdd is called when a pgtask is added
func (c *Controller) onAdd(obj interface{}) {
	task := obj.(*crv1.Pgtask)

	//handle the case of when the operator restarts, we do not want
	//to process pgtasks already processed
	if task.Status.State == crv1.PgtaskStateProcessed {
		log.Debug("pgtask " + task.ObjectMeta.Name + " already processed")
		return
	}

	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err == nil {
		log.Debugf("task putting key in queue %s", key)
		c.Queue.Add(key)
	}

}

// onUpdate is called when a pgtask is updated
func (c *Controller) onUpdate(oldObj, newObj interface{}) {
	//task := newObj.(*crv1.Pgtask)
	//	log.Debugf("[Controller] onUpdate ns=%s %s", task.ObjectMeta.Namespace, task.ObjectMeta.SelfLink)
}

// onDelete is called when a pgtask is deleted
func (c *Controller) onDelete(obj interface{}) {
}

// AddPGTaskEventHandler adds the pgtask event handler to the pgtask informer
func (c *Controller) AddPGTaskEventHandler() {

	c.Informer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onAdd,
		UpdateFunc: c.onUpdate,
		DeleteFunc: c.onDelete,
	})

	log.Debugf("pgtask Controller: added event handler to informer")
}

//de-dupe logic for a failover, if the failover started
//parameter is set, it means a failover has already been
//started on this
func dupeFailover(restClient *rest.RESTClient, task *crv1.Pgtask, ns string) bool {
	tmp := crv1.Pgtask{}

	found, _ := kubeapi.Getpgtask(restClient, &tmp, task.Spec.Name, ns)
	if !found {
		//a big time error if this occurs
		return false
	}

	if tmp.Spec.Parameters[config.LABEL_FAILOVER_STARTED] == "" {
		return false
	}

	return true
}

//de-dupe logic for a delete data, if the delete data job started
//parameter is set, it means a delete data job has already been
//started on this
func dupeDeleteData(restClient *rest.RESTClient, task *crv1.Pgtask, ns string) bool {
	tmp := crv1.Pgtask{}

	found, _ := kubeapi.Getpgtask(restClient, &tmp, task.Spec.Name, ns)
	if !found {
		//a big time error if this occurs
		return false
	}

	if tmp.Spec.Parameters[config.LABEL_DELETE_DATA_STARTED] == "" {
		return false
	}

	return true
}

// WorkerCount returns the worker count for the controller
func (c *Controller) WorkerCount() int {
	return c.PgtaskWorkerCount
}
