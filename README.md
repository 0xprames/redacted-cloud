# Redacted-cloud


## Background
This package implements a light-weight "Redacted" control plane; The control plane (aka worker) handles client requests to Create/Delete "Redacted" clusters. The driver/worker were deployed on a Minikube cluster (latest version/default addons i.e running CoreDNS instead of Kube-DNS etc) on an M1 Mac, with a cluster-admin role-binding associated with the default service-account. The driver pod was manually restarted many times over a period of ~10 hours and it was validated that its requests were served correctly.

### Deployment
```
# from the package root run:
make docker
kubectl label nodes <your-nodename-here> Redacted=subtask-topology-aware # pods get stuck in Pending, and CreateRedacted will eventually timeout without this step
kubectl apply -f deploy/ # note that this will deploy both pods into the default namespace, which the code currently requires to run correctly. 
# wait for ~30-60 seconds for the pods to come up, gRPC requests to get sent (on initial worker start the driver waits on DNS to be ready which takes ~30 seconds as per grpc-go code), etc.
# the driver will make a Create request for a 30 replica cluster and block, the worker will service this request and wait for the deployment to be available before responding
# after the worker responds with a success, the driver will then kick off the delete and block on a response - at which point the deployment will be deleted
kubectl logs Redacted-driver 
# should show:
#time="2023-01-23T14:20:19Z" level=info msg="Creating Redacted cluster"
#time="2023-01-23T14:20:44Z" level=info msg="Created Redacted cluster"
#time="2023-01-23T14:20:44Z" level=info msg="Deleting Redacted cluster"
#time="2023-01-23T14:20:44Z" level=info msg="Deleted Redacted cluster
```

### Implementation details

The worker is implemented as a gRPC service that offers `CreateRedacted` and `DeleteRedacted` RPC methods. The methods' params/return values follow the challenge prompt - the service definition along with the request/response objects are defined in proto/Redacted.proto. `CreateRedacted` triggers the worker to instantiate a "Redacted" cluster as a Kubernetes Deployment, and `DeleteRedacted` is handled by deleting the associated Kubernetes deployment.

The `CreateRedacted` and `DeleteRedacted` requests are handled asynchronously "natively" through gRPC's `Serve`, so I didn't need to implement any special handler logic for them.
Both the Create and Delete methods follow a similar design pattern to satisfy the challenge prompt - namely, that each method should wait for a "success" state from Kubernetes before returning a response.

Getting back to the worker implementation - the general logic for handling both Creates/Deletes is as follows:

```
1) Request is recieved  
2) Clustername from request is checked against its current state in worker's clusterInfo store - this is meant to handle asynchronous (and out-of-order/repeated/concurrent etc) Creates/Deletes that come in for a single resource. I.e if a Create for the cluster comes in while the cluster is already "CREATE_IN_PROGRES" the worker returns an "IN_PROGRESS" status, or if a Delete comes in for a cluster before any Creates come in - it would return a "Not Found" error.   
3) If the above checks succeed, the worker calls Kubernetes to Create/Delete the deployment.
4) The request routine is setup to then block on a state Channel (one for Creates and one for Deletes) that is created per cluster - and waits for an update on the deployment it Created/Deleted.
5) In the "background" (running in a goroutine) - a Kubernetes Informer works to watch updates to all "Redacted" deployments - and send state transition updates back to the request routine via the cluster's state channel
6) The request routine either recieves an update from the Deployment Informer - or times out
```

The worker (and the driver - or process that sends Create/Delete calls) is deployed on Kubernetes as a pod. It can be reached by other pods at its Fully-Qualified-Domain-Name, which is configured in the workers' Pod manifest - The FQDN is currently set to `worker.Redacted.default.svc.cluster.local`. It's good to note here that Kubernetes pods by default do not get an A-record for their FQDN, I've implemented this by using a Headless Service called `Redacted` in the same namespace as the pods (default), and having the worker pod's subdomain also be `Redacted`. This setup enables the cluster DNS to craete A/AAAA records for the pods FQDN.