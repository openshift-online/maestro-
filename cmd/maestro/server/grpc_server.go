package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strings"

	ce "github.com/cloudevents/sdk-go/v2"
	"github.com/cloudevents/sdk-go/v2/binding"
	cetypes "github.com/cloudevents/sdk-go/v2/types"
	"github.com/golang/glog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/types/known/emptypb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubetypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	workv1 "open-cluster-management.io/api/work/v1"
	pbv1 "open-cluster-management.io/sdk-go/pkg/cloudevents/generic/options/grpc/protobuf/v1"
	grpcprotocol "open-cluster-management.io/sdk-go/pkg/cloudevents/generic/options/grpc/protocol"
	"open-cluster-management.io/sdk-go/pkg/cloudevents/generic/payload"
	"open-cluster-management.io/sdk-go/pkg/cloudevents/generic/types"
	"open-cluster-management.io/sdk-go/pkg/cloudevents/work/common"
	workpayload "open-cluster-management.io/sdk-go/pkg/cloudevents/work/payload"

	"github.com/openshift-online/maestro/pkg/api"
	"github.com/openshift-online/maestro/pkg/client/cloudevents"
	"github.com/openshift-online/maestro/pkg/config"
	"github.com/openshift-online/maestro/pkg/event"
	"github.com/openshift-online/maestro/pkg/services"
)

const subscriptionRequestTopicPattern = `^sources/([a-z0-9-]+)/clusters/([a-z0-9-]+|\+)/status$`

// GRPCServer includes a gRPC server and a resource service
type GRPCServer struct {
	pbv1.UnimplementedCloudEventServiceServer
	grpcServer       *grpc.Server
	eventBroadcaster *event.EventBroadcaster
	resourceService  services.ResourceService
	bindAddress      string
}

// NewGRPCServer creates a new GRPCServer
func NewGRPCServer(resourceService services.ResourceService, eventBroadcaster *event.EventBroadcaster, config config.GRPCServerConfig) *GRPCServer {
	grpcServerOptions := make([]grpc.ServerOption, 0)
	grpcServerOptions = append(grpcServerOptions, grpc.MaxRecvMsgSize(config.MaxReceiveMessageSize))
	grpcServerOptions = append(grpcServerOptions, grpc.MaxSendMsgSize(config.MaxSendMessageSize))
	grpcServerOptions = append(grpcServerOptions, grpc.MaxConcurrentStreams(config.MaxConcurrentStreams))
	grpcServerOptions = append(grpcServerOptions, grpc.ConnectionTimeout(config.ConnectionTimeout))
	grpcServerOptions = append(grpcServerOptions, grpc.WriteBufferSize(config.WriteBufferSize))
	grpcServerOptions = append(grpcServerOptions, grpc.ReadBufferSize(config.ReadBufferSize))
	grpcServerOptions = append(grpcServerOptions, grpc.KeepaliveParams(keepalive.ServerParameters{
		MaxConnectionAge: config.MaxConnectionAge,
	}))

	if config.EnableTLS {
		// Check tls cert and key path path
		if config.TLSCertFile == "" || config.TLSKeyFile == "" {
			check(
				fmt.Errorf("unspecified required --grpc-tls-cert-file, --grpc-tls-key-file"),
				"Can't start gRPC server",
			)
		}

		// Serve with TLS
		creds, err := credentials.NewServerTLSFromFile(config.TLSCertFile, config.TLSKeyFile)
		if err != nil {
			glog.Fatalf("Failed to generate credentials %v", err)
		}
		grpcServerOptions = append(grpcServerOptions, grpc.Creds(creds))
		glog.Infof("Serving gRPC service with TLS at %s", config.BindPort)
	} else {
		glog.Infof("Serving gRPC service without TLS at %s", config.BindPort)
	}

	return &GRPCServer{
		grpcServer:       grpc.NewServer(grpcServerOptions...),
		eventBroadcaster: eventBroadcaster,
		resourceService:  resourceService,
		bindAddress:      env().Config.HTTPServer.Hostname + ":" + config.BindPort,
	}
}

// Start starts the gRPC server
func (svr *GRPCServer) Start() error {
	lis, err := net.Listen("tcp", svr.bindAddress)
	if err != nil {
		glog.Fatalf("failed to listen: %v", err)
		return err
	}
	pbv1.RegisterCloudEventServiceServer(svr.grpcServer, svr)
	return svr.grpcServer.Serve(lis)
}

// Stop stops the gRPC server
func (svr *GRPCServer) Stop() {
	svr.grpcServer.GracefulStop()
}

// Publish implements the Publish method of the CloudEventServiceServer interface
func (svr *GRPCServer) Publish(ctx context.Context, pubReq *pbv1.PublishRequest) (*emptypb.Empty, error) {
	// WARNING: don't use "evt, err := pb.FromProto(pubReq.Event)" to convert protobuf to cloudevent
	evt, err := binding.ToEvent(ctx, grpcprotocol.NewMessage(pubReq.Event))
	if err != nil {
		return nil, fmt.Errorf("failed to convert protobuf to cloudevent: %v", err)
	}

	eventType, err := types.ParseCloudEventsType(evt.Type())
	if err != nil {
		return nil, fmt.Errorf("failed to parse cloud event type %s, %v", evt.Type(), err)
	}

	// handler resync request
	if eventType.Action == types.ResyncRequestAction {
		err := svr.respondResyncStatusRequest(ctx, evt)
		if err != nil {
			return nil, fmt.Errorf("failed to respond resync status request: %v", err)
		}
		return &emptypb.Empty{}, nil
	}

	res, err := decode(eventType.CloudEventsDataType, evt)
	if err != nil {
		return nil, fmt.Errorf("failed to decode cloudevent: %v", err)
	}

	switch eventType.Action {
	case common.CreateRequestAction:
		_, err := svr.resourceService.Create(ctx, res)
		if err != nil {
			return nil, fmt.Errorf("failed to create resource: %v", err)
		}
	case common.UpdateRequestAction:
		if res.HasManifestBundle() {
			found, err := svr.resourceService.Get(ctx, res.ID)
			if err != nil {
				return nil, fmt.Errorf("failed to get resource: %v", err)
			}
			// handle the special case that the resource is updated by the source controller
			// and the version of the resource in the request is less than it in the database
			if found.Version < res.Version {
				res.Version = found.Version
			}
		}
		_, err := svr.resourceService.Update(ctx, res)
		if err != nil {
			return nil, fmt.Errorf("failed to update resource: %v", err)
		}
	case common.DeleteRequestAction:
		err := svr.resourceService.MarkAsDeleting(ctx, res.ID)
		if err != nil {
			return nil, fmt.Errorf("failed to update resource: %v", err)
		}
	default:
		return nil, fmt.Errorf("unsupported action %s", eventType.Action)
	}

	return &emptypb.Empty{}, nil
}

// Subscribe implements the Subscribe method of the CloudEventServiceServer interface
func (svr *GRPCServer) Subscribe(subReq *pbv1.SubscriptionRequest, subServer pbv1.CloudEventService_SubscribeServer) error {
	if !regexp.MustCompile(subscriptionRequestTopicPattern).MatchString(subReq.Topic) {
		return fmt.Errorf("invalid subscription topic %q, it should match `%s`", subReq.Topic, subscriptionRequestTopicPattern)
	}

	topicSplits := strings.Split(subReq.Topic, "/")
	if len(topicSplits) != 5 {
		return fmt.Errorf("invalid subscription topic %s", subReq.Topic)
	}

	source, clusterName := topicSplits[1], topicSplits[3]

	clientID, errChan := svr.eventBroadcaster.Register(source, clusterName, func(res *api.Resource) error {
		evt, err := encode(res)
		if err != nil {
			return fmt.Errorf("failed to encode resource %s to cloudevent: %v", res.ID, err)
		}

		// WARNING: don't use "pbEvt, err := pb.ToProto(evt)" to convert cloudevent to protobuf
		pbEvt := &pbv1.CloudEvent{}
		if err = grpcprotocol.WritePBMessage(context.TODO(), binding.ToMessage(evt), pbEvt); err != nil {
			return fmt.Errorf("failed to convert cloudevent to protobuf: %v", err)
		}

		// send the cloudevent to the subscriber
		// TODO: error handling to address errors beyond network issues.
		if err := subServer.Send(pbEvt); err != nil {
			return err
		}

		return nil
	})

	select {
	case err := <-errChan:
		svr.eventBroadcaster.Unregister(clientID)
		return err
	case <-subServer.Context().Done():
		svr.eventBroadcaster.Unregister(clientID)
		return nil
	}
}

// decode translates a cloudevent to a resource
func decode(eventDataType types.CloudEventsDataType, evt *ce.Event) (*api.Resource, error) {
	evtExtensions := evt.Context.GetExtensions()

	clusterName, err := cetypes.ToString(evtExtensions[types.ExtensionClusterName])
	if err != nil {
		return nil, fmt.Errorf("failed to get clustername extension: %v", err)
	}

	resourceID, err := cetypes.ToString(evtExtensions[types.ExtensionResourceID])
	if err != nil {
		return nil, fmt.Errorf("failed to get resourceid extension: %v", err)
	}

	resourceVersion, err := cetypes.ToInteger(evtExtensions[types.ExtensionResourceVersion])
	if err != nil {
		return nil, fmt.Errorf("failed to get resourceversion extension: %v", err)
	}

	resource := &api.Resource{
		Source:     evt.Source(),
		ConsumerID: clusterName,
		Version:    int32(resourceVersion),
		Meta: api.Meta{
			ID: resourceID,
		},
	}

	if deletionTimestampValue, exists := evtExtensions[types.ExtensionDeletionTimestamp]; exists {
		deletionTimestamp, err := cetypes.ToTime(deletionTimestampValue)
		if err != nil {
			return nil, fmt.Errorf("failed to convert deletion timestamp %v to time.Time: %v", deletionTimestampValue, err)
		}
		resource.Meta.DeletedAt.Time = deletionTimestamp
	}

	switch eventDataType {
	case workpayload.ManifestEventDataType:
		manifest := &workpayload.Manifest{}
		if err := evt.DataAs(manifest); err != nil {
			return nil, fmt.Errorf("failed to unmarshal event data %s, %v", string(evt.Data()), err)
		}

		resource.Manifest = manifest.Manifest.Object
	case workpayload.ManifestBundleEventDataType:
		manifestBundle := &workpayload.ManifestBundle{}
		if err := evt.DataAs(manifestBundle); err != nil {
			return nil, fmt.Errorf("failed to unmarshal event data %s, %v", string(evt.Data()), err)
		}

		work := &workv1.ManifestWork{
			TypeMeta: metav1.TypeMeta{
				Kind:       "ManifestWork",
				APIVersion: workv1.GroupVersion.String(),
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: resourceID,
				UID:  kubetypes.UID(resourceID),
			},
			Spec: workv1.ManifestWorkSpec{
				Workload: workv1.ManifestsTemplate{
					Manifests: manifestBundle.Manifests,
				},
				DeleteOption:    manifestBundle.DeleteOption,
				ManifestConfigs: manifestBundle.ManifestConfigs,
			},
		}

		workJSON, err := json.Marshal(work)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal manifestwork: %v", err)
		}
		err = json.Unmarshal(workJSON, &resource.Manifest)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal manifestwork: %v", err)
		}
	default:
		return nil, fmt.Errorf("unsupported cloudevents data type %s", eventDataType)
	}

	return resource, nil
}

// encode translates a resource to a cloudevent
func encode(resource *api.Resource) (*ce.Event, error) {
	eventType := types.CloudEventsType{
		CloudEventsDataType: workpayload.ManifestEventDataType,
		SubResource:         types.SubResourceStatus,
		Action:              config.UpdateRequestAction,
	}
	if resource.HasManifestBundle() {
		eventType.CloudEventsDataType = workpayload.ManifestBundleEventDataType
	}

	evt := types.NewEventBuilder(resource.Source, eventType).
		WithResourceID(resource.ID).
		WithResourceVersion(int64(resource.Version)).
		WithClusterName(resource.ConsumerID).
		WithOriginalSource(resource.Source).
		NewEvent()

	resourceStatus, err := api.JSONMapStatusToResourceStatus(resource.Status)
	if err != nil {
		return nil, fmt.Errorf("failed to convert resource status to resource status: %v", err)
	}

	if resource.HasManifestBundle() {
		statusPayload := &workpayload.ManifestBundleStatus{}
		if resourceStatus.ReconcileStatus != nil {
			statusPayload.Conditions = resourceStatus.ReconcileStatus.Conditions
		}

		contentStatusMap := map[string]interface{}(resourceStatus.ContentStatus)
		manifestStatus, ok := contentStatusMap["ManifestStatus"]
		if !ok {
			return nil, fmt.Errorf("resource status not found in content status")
		}
		manifestStatusJSON, err := json.Marshal(manifestStatus)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal content status, %v", err)
		}
		err = json.Unmarshal(manifestStatusJSON, &statusPayload.ResourceStatus)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal content status, %v", err)
		}

		if err := evt.SetData(ce.ApplicationJSON, statusPayload); err != nil {
			return nil, fmt.Errorf("failed to encode manifestwork status to cloudevent: %v", err)
		}
	} else {
		contentStatusJSON, err := json.Marshal(resourceStatus.ContentStatus)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal content status, %v", err)
		}
		contentStatusJSONStr := string(contentStatusJSON)

		statusPayload := &workpayload.ManifestStatus{
			Conditions: resourceStatus.ReconcileStatus.Conditions,
			Status: &workv1.ManifestCondition{
				Conditions: resourceStatus.ReconcileStatus.Conditions,
				StatusFeedbacks: workv1.StatusFeedbackResult{
					Values: []workv1.FeedbackValue{
						{
							Name: "status",
							Value: workv1.FieldValue{
								Type:    workv1.JsonRaw,
								JsonRaw: &contentStatusJSONStr,
							},
						},
					},
				},
			},
		}

		if err := evt.SetData(ce.ApplicationJSON, statusPayload); err != nil {
			return nil, fmt.Errorf("failed to encode manifestwork status to cloudevent: %v", err)
		}
	}

	return &evt, nil
}

// respondResyncStatusRequest responds to the status resync request by comparing the status hash of the resources
// from the database and the status hash in the request, and then respond the resources whose status is changed.
func (svr *GRPCServer) respondResyncStatusRequest(ctx context.Context, evt *ce.Event) error {
	objs, serviceErr := svr.resourceService.FindBySource(ctx, evt.Source())
	if serviceErr != nil {
		return fmt.Errorf("failed to list resources: %s", serviceErr)
	}

	statusHashes, err := payload.DecodeStatusResyncRequest(*evt)
	if err != nil {
		return fmt.Errorf("failed to decode status resync request: %v", err)
	}

	if len(statusHashes.Hashes) == 0 {
		// publish all resources status
		for _, obj := range objs {
			svr.eventBroadcaster.Broadcast(obj)
		}

		return nil
	}

	for _, obj := range objs {
		lastHash, ok := findStatusHash(string(obj.GetUID()), statusHashes.Hashes)
		if !ok {
			// ignore the resource that is not on the source, but exists on the maestro, wait for the source deleting it
			klog.Infof("The resource %s is not found from the maestro, ignore", obj.GetUID())
			continue
		}

		currentHash, err := cloudevents.ResourceStatusHashGetter(obj)
		if err != nil {
			continue
		}

		if currentHash == lastHash {
			// the status is not changed, do nothing
			continue
		}

		svr.eventBroadcaster.Broadcast(obj)
	}

	return nil
}

// findStatusHash finds the status hash of the resource from the status resync request payload
func findStatusHash(id string, hashes []payload.ResourceStatusHash) (string, bool) {
	for _, hash := range hashes {
		if id == hash.ResourceID {
			return hash.StatusHash, true
		}
	}

	return "", false
}
