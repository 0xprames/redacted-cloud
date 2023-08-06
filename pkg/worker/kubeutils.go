package worker

import (
	log "github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	v1 "k8s.io/client-go/kubernetes/typed/apps/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

func getDeploymentSpec(name string, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"owner": "Redacted-worker",
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"owner": "Redacted-worker",
				},
			},
			Template: apiv1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"owner": "Redacted-worker",
					},
				},
				Spec: apiv1.PodSpec{
					Containers: []apiv1.Container{
						{
							Name:  "Redacted",
							Image: "nginx:latest",
							Ports: []apiv1.ContainerPort{
								{
									Name:          "http",
									Protocol:      apiv1.ProtocolTCP,
									ContainerPort: 80,
								},
							},
						},
					},
					NodeSelector: map[string]string{
						"Redacted": "subtask-topology-aware",
					},
				},
			},
		},
	}
}

func getKubeClients() (*kubernetes.Clientset, v1.DeploymentInterface, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		// we should always be running in a cluster - an error here usually means the worker is not deployed inside a k8s cluster
		return nil, nil, err
	}
	kc, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, err
	}
	// no namespace requirements in code challenge prompt - leave as default namespace for now
	depClient := kc.AppsV1().Deployments(apiv1.NamespaceDefault)
	return kc, depClient, nil
}

func initializeInformer(server *WorkerServer) {
	factory := informers.NewSharedInformerFactory(server.kube, 0)
	informer := factory.Apps().V1().Deployments().Informer()
	addEventHandlers(informer, server)
	//go informer.Run(server.shutdown) -> causes a deadlock within the informer in WaitForCacheSync - causing us to not recieve events and timeout create/delete reqs
	// we should not do both go informer.Run() and factory.Start(), and we should also not go factory.Start() since internally it spawns its own goroutine that is "synced" with WaitForCacheSync
	factory.Start(server.shutdown)
	factory.WaitForCacheSync(server.shutdown)
}

// helper func to flesh out the deployment-informer's handlers for add/update/delete events
func addEventHandlers(informer cache.SharedIndexInformer, server *WorkerServer) {
	informer.AddEventHandler(
		cache.FilteringResourceEventHandler{
			// filter out specifically for "our" deployments - deployments created by the worker will have the "owner: Redacted-worker" label
			FilterFunc: func(obj interface{}) bool {
				switch t := obj.(type) {
				case *appsv1.Deployment:
					if val, ok := t.Labels["owner"]; ok {
						// nested if because we want to be safe and reference val if its present - not sure what default value is for nonexistent keys
						if val == "Redacted-worker" {
							return true
						}
					}
					return false
				case cache.DeletedFinalStateUnknown:
					if obj, ok := t.Obj.(*appsv1.Deployment); ok {
						if val, ok := obj.Labels["owner"]; ok {
							if val == "Redacted-worker" {
								return true
							}
						}
					}
					log.Error("Informer: Failed to convert object to Deployment: %v", obj)
					return false
				default:
					log.Errorf("Informer: Could not convert object to Deployment: %v", obj)
					return false
				}
			},
			Handler: cache.ResourceEventHandlerFuncs{
				AddFunc: func(obj interface{}) {
					deployment, ok := obj.(*appsv1.Deployment)
					if !ok {
						log.Error("AddHandler: Failed to convert object to Deployment: %v", obj)
						return
					}
					server.processState(deployment, "ADD")
				},
				UpdateFunc: func(_ interface{}, obj interface{}) {
					// we get flooded with Update events here, that sometimes have no real update.
					// TODO: see if olObj can be leveraged to check for diffs even before we processState
					deployment, ok := obj.(*appsv1.Deployment)
					if !ok {
						log.Error("UpdateHandler: Failed to convert object to Deployment: %v", obj)
						return
					}
					server.processState(deployment, "UPDATE")
				},
				DeleteFunc: func(obj interface{}) {
					deployment, ok := obj.(*appsv1.Deployment)
					if !ok {
						log.Error("DeleteHandler: Failed to convert object to Deployment: %v", obj)
						return
					}
					server.processState(deployment, "DELETE")
				},
			},
		},
	)
}

/*
processState

	handles the state transitions for a given cluster - sends the state via stateCh for it to be read and written into clusterInfo by the request goroutine
	we don't want to explicitly write into clusterInfo here mainly to isolate write access to the create/delete request goroutine
	if this func wrote directly into clusterInfo - the request routine would have to poll the status in the clusterInfo struct
	instead of just waiting on a notification that the status has been update
*/
func (s *WorkerServer) processState(deployment *appsv1.Deployment, event string) {
	name := deployment.Name
	log.Printf("Inside processState for %s with event %s", name, event)
	s.lock.RLock()
	if info, exists := s.clusterInfo[name]; exists {
		s.lock.RUnlock() // unlock here to avoid the deadlock while processing CREATE_DONE
		switch event {
		case "ADD":
			if info.actionStatus != "SENT" {
				// the informer has already processed this deployment and updated the status of the cluster
				return
			}
			if info.lastAction == "CREATE" {
				// this check guards against out of order events, and when the informers cache gets resynced on pod restart
				// we dont want to issue a create in progress state if the cluster is currently being deleted
				info.createCh <- "CREATE_IN PROGRESS"
			}
			return
		case "UPDATE":
			// no guarantees on event ordering from k8s - check state of cluster on our end
			currentInfo := s.clusterInfo[name]
			isCreating := currentInfo.lastAction == "CREATE" && currentInfo.actionStatus != "COMPLETE" // this can be stale, resulting in one extra CREATE_DONE
			if isDeploymentAvailable(deployment) && isCreating {
				info.createCh <- "CREATE_DONE"
			} else if !isCreating {
				// this occurs in K8s and is normal - the events here can be used to heartbeat a given cluster
				log.Printf("Recieved update event for %s with status %s after create completion - returning", deployment.Name, info.actionStatus)
			}
			return
		case "DELETE":
			// with pod deletes there are some nuances around when kubelet signals a delete to the APIserver and the grace period (see Pod termination lifecycle in k8s docs)
			// this causes update events to fire after a pod delete has been issued
			// deployment deletes don't suffer from the same complexity - return success once the OnDelete is called by the informer
			log.Printf("Deployment was deleted: %s", deployment.Name)
			info.deleteCh <- "DELETE_DONE"
			return
		default:
			log.Printf("Unknown handler %s being called on deployment %v", event, deployment)
			return

		}
	} else {
		s.lock.RUnlock()
	}
}

func isDeploymentAvailable(dep *appsv1.Deployment) bool {
	conditions := dep.Status.Conditions
	for _, condition := range conditions {
		if condition.Type == appsv1.DeploymentAvailable && condition.Status == apiv1.ConditionTrue {
			// TODO: check lastUpdatedTime here for cases where deployment has a stale Available condition potentially
			// TODO: deployments become available when min replicas is met - do we want to only return true when all replicas are up?
			return true
		}
	}
	return false
}
