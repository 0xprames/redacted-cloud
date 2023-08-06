package worker

import (
	"Redacted-cloud-challenge/proto"
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/sasha-s/go-deadlock"
	log "github.com/sirupsen/logrus"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	v1 "k8s.io/client-go/kubernetes/typed/apps/v1"
)

type WorkerServer struct {
	proto.UnimplementedRedactedServer
	kube        *kubernetes.Clientset
	depClient   v1.DeploymentInterface
	clusterInfo map[string]*clusterInfo // clusterInfo is used to track cluster state - at EKS on the control plane we used DDB for this, and K8s uses etcd similarly
	lock        *deadlock.RWMutex
	shutdown    chan struct{}
}

type clusterInfo struct {
	lastAction   string      // CREATE/DELETE
	actionStatus string      // could be an enum - SENT/IN PROGRESS(-ing states from K8s)/COMPLETE
	createCh     chan string // used to pass create status messages about a cluster from the deployment informer (K8s API) to the create
	deleteCh     chan string // used to pass create status messages about a cluster from the deployment informer (K8s API) to the delete handler
}

func NewWorkerServer(stopCh chan struct{}) proto.RedactedServer {
	// setup internal data structures to track cluster state - in prod these would ideally be their own services running outside of the worker
	info := make(map[string]*clusterInfo)
	mutex := &deadlock.RWMutex{}
	s := &WorkerServer{clusterInfo: info, lock: mutex, shutdown: stopCh}
	kc, dc, err := getKubeClients()
	if err != nil {
		log.Fatalf("Failed to get in-cluster Kubernetes Client: %v", err)
	}
	s.kube = kc
	s.depClient = dc
	// setup deployment informers
	initializeInformer(s)
	return s
}

// Server functions are asynchronously called in separate goroutines while being handled via the grpc Serve implementation
func (s *WorkerServer) CreateRedacted(ctx context.Context, createReq *proto.CreateRedactedRequest) (*proto.CreateRedactedResponse, error) {
	// fields are not optional in protobuffer - assume valid reqs for now, TODO add some checks
	org := createReq.OrgId
	cluster := createReq.RedactedClusterId
	// TODO: overflow error handling
	replicas := int32(createReq.Replicas)

	name := strconv.Itoa(int(org)) + "-" + strconv.Itoa(int(cluster)) + "-Redacted-cluster"
	deployment := getDeploymentSpec(name, replicas)
	log.Println("Creating Deployment: %s", name)
	var createCh chan string
	var deleteCh chan string
	// create a cluster status entry in our store
	s.lock.RLock()
	if info, exists := s.clusterInfo[name]; exists {
		s.lock.RUnlock()
		// cluster exists and has not been deleted
		if info.lastAction == "CREATE" {
			// Could TEST here
			var status = "SUCCESS"
			if info.actionStatus != "COMPLETE" {
				// duplicate create request, return a response with "IN PROGRESS"
				status = "IN PROGRESS"
			}
			return &proto.CreateRedactedResponse{Status: status, OrgId: org, RedactedClusterId: cluster}, nil
		} else {
			// log error because we should never have left this in our store - don't Fatal out here because this could be an issue with the data store (e.g if the store was remote and ran into a sev2 causing stale data)
			log.Errorf("Cluster %s exists in a bad state: %v", name, info)
		}
	} else {
		s.lock.RUnlock()
		// initialize cluster in our store
		createCh = make(chan string)
		deleteCh = make(chan string)
		s.lock.Lock()
		s.clusterInfo[name] = &clusterInfo{lastAction: "CREATE", actionStatus: "SENT", createCh: createCh, deleteCh: deleteCh}
		s.lock.Unlock()
	}
	// TODO: check if using the grpc req context here makes sense?
	_, err := s.depClient.Create(ctx, deployment, metav1.CreateOptions{})
	if err != nil {
		log.Errorf("Error creating deployment %s, %v", deployment.Name, err)
		// cleanup cluster from our datastore - delete is a no-op for nonexistent key
		s.lock.Lock()
		delete(s.clusterInfo, name)
		s.lock.Unlock()
		return &proto.CreateRedactedResponse{Status: "Error", OrgId: org, RedactedClusterId: cluster}, err
	}
	// now we wait to see if/when the deployment succeeds
	// the timeout assumption here may not be correct for large deployments, since we wait until K8s tells us the deployment has enough minimalAvailableReplicas - for now, we wait for a minute
	timeout := time.After(60 * time.Second)
	var res *proto.CreateRedactedResponse
loop:
	for {
		select {
		case status, ok := <-createCh:
			if !ok {
				break loop
			}
			log.Printf("Recieved state update: %s", status)
			if status == "CREATE_IN PROGRESS" {
				s.updateActionStatus(name, "IN PROGRESS")
			} else if status == "CREATE_ERROR" {
				s.updateActionStatus(name, "ERROR")
				res = &proto.CreateRedactedResponse{Status: "ERROR", OrgId: org, RedactedClusterId: cluster}
				err = fmt.Errorf(("error creating cluster: %v"), status)
				break loop
			} else {
				log.Printf("We're done with: %s", status)
				s.updateActionStatus(name, "COMPLETE")
				res = &proto.CreateRedactedResponse{Status: "SUCCESS", OrgId: org, RedactedClusterId: cluster}
				err = nil
				break loop
			}
		case <-timeout:
			log.Printf("Timed out creating cluster: %s", name)
			s.updateActionStatus(name, "ERROR")
			res = &proto.CreateRedactedResponse{Status: "ERROR", OrgId: org, RedactedClusterId: cluster}
			err = fmt.Errorf(("error creating cluster: timed out"))
			break loop
		}
	}
	log.Printf("Returning create response: %v", res)
	return res, err
}

func (s *WorkerServer) DeleteRedacted(ctx context.Context, req *proto.DeleteRedactedRequest) (*proto.DeleteRedactedResponse, error) {
	// fields are not optional in protobuffer - assume valid reqs for now, TODO add some checks
	org := req.OrgId
	cluster := req.RedactedClusterId
	name := generateClusterName(org, cluster)
	var stateCh chan string
	s.lock.RLock()
	if info, exists := s.clusterInfo[name]; !exists {
		s.lock.RUnlock()
		return &proto.DeleteRedactedResponse{Status: "ERROR", OrgId: org, RedactedClusterId: cluster}, fmt.Errorf("error deleting cluster %s: Not Found", name)
	} else {
		s.lock.RUnlock()
		stateCh = info.deleteCh
		s.lock.Lock()
		s.clusterInfo[name].lastAction = "DELETE"
		s.clusterInfo[name].actionStatus = "SENT"
		s.lock.Unlock()
	}
	log.Printf("Calling delete cluster for %s", name)
	depClient := s.depClient
	err := depClient.Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		s.updateActionStatus(name, "ERROR")
		return &proto.DeleteRedactedResponse{Status: "ERROR", OrgId: org, RedactedClusterId: cluster}, fmt.Errorf("error deleting cluster %s: %v", name, err)
	}
	// now we wait to see if/when the deployment succeeds
	// the timeout assumption here may not be correct for large deployment
	timeout := time.After(60 * time.Second)
	var res *proto.DeleteRedactedResponse
loop:
	for {
		select {
		case status, ok := <-stateCh:
			if !ok {
				break loop
			}
			log.Printf("DeleteRedacted: Recieved state update: %s", status)
			if status == "DELETE_DONE" {
				s.updateActionStatus(name, "COMPLETE")
				s.lock.Lock()
				delete(s.clusterInfo, name)
				s.lock.Unlock()
				res = &proto.DeleteRedactedResponse{Status: "SUCCESS", OrgId: org, RedactedClusterId: cluster}
				err = nil
				break loop
			} // possible to recieve a duplicates CREATE_DONE here that is still being processed, dont do anything since we've already returned the create
		case <-timeout:
			log.Printf("Timed out deleting cluster: %s", name) // return success here regardless since the timeouts with Delete can only caused by K8s informer deadlocks,
			// and the apiserver still deletes the deployment instantaneously - with Pods this may be a different story due to Kubelet, but we should be OK assuming timeouts don't mean the deployment actually wasn't deleted
			s.lock.Lock()
			delete(s.clusterInfo, name)
			s.lock.Unlock()
			res = &proto.DeleteRedactedResponse{Status: "SUCCESS", OrgId: org, RedactedClusterId: cluster}
			err = nil
			break loop
		}
	}
	log.Printf("Returning delete response: %v", res)
	return res, err
}

func (s *WorkerServer) updateActionStatus(name string, actionStatus string) {
	log.Println("Grabbing lock")
	s.lock.Lock()
	log.Println("Inside lock")
	s.clusterInfo[name].actionStatus = actionStatus
	log.Println("Done updating action status")
	s.lock.Unlock()
	log.Println("Outside lock")
}

func generateClusterName(orgId, clusterId int64) string {
	return strconv.Itoa(int(orgId)) + "-" + strconv.Itoa(int(clusterId)) + "-Redacted-cluster"
}
