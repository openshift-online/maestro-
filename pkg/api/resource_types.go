package api

import (
	"encoding/json"
	"fmt"
	"strconv"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	cloudeventstypes "github.com/cloudevents/sdk-go/v2/types"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ktypes "k8s.io/apimachinery/pkg/types"

	workv1 "open-cluster-management.io/api/work/v1"
	cetypes "open-cluster-management.io/sdk-go/pkg/cloudevents/generic/types"
	workpayload "open-cluster-management.io/sdk-go/pkg/cloudevents/work/payload"
)

type ResourceType string

const (
	ResourceTypeSingle ResourceType = "Single"
	ResourceTypeBundle ResourceType = "Bundle"
)

type Resource struct {
	Meta
	Version    int32
	Source     string
	ConsumerID string
	Type       ResourceType
	Manifest   datatypes.JSONMap
	Status     datatypes.JSONMap
}

type ResourceStatus struct {
	ContentStatus   datatypes.JSONMap
	ReconcileStatus *ReconcileStatus
}

type ReconcileStatus struct {
	ObservedVersion int32
	SequenceID      string
	Conditions      []metav1.Condition
}

type ResourceList []*Resource
type ResourceIndex map[string]*Resource

func (l ResourceList) Index() ResourceIndex {
	index := ResourceIndex{}
	for _, o := range l {
		index[o.ID] = o
	}
	return index
}

func (d *Resource) BeforeCreate(tx *gorm.DB) error {
	// generate a new ID if it doesn't exist
	if d.ID == "" {
		d.ID = NewID()
	}
	return nil
}

func (d *Resource) GetUID() ktypes.UID {
	return ktypes.UID(d.Meta.ID)
}

func (d *Resource) GetResourceVersion() string {
	return strconv.FormatInt(int64(d.Version), 10)
}

func (d *Resource) GetDeletionTimestamp() *metav1.Time {
	return &metav1.Time{Time: d.Meta.DeletedAt.Time}
}

type ResourcePatchRequest struct{}

// JSONMAPToCloudEvent converts a JSONMap (resource manifest or status) to a CloudEvent
func JSONMAPToCloudEvent(res datatypes.JSONMap) (*cloudevents.Event, error) {
	resJSON, err := res.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal JSONMAP to cloudevent JSON: %v", err)
	}

	evt := &cloudevents.Event{}
	if err := json.Unmarshal(resJSON, evt); err != nil {
		return nil, fmt.Errorf("failed to unmarshal JSONMAP to cloudevent: %v", err)
	}

	return evt, nil
}

// CloudEventToJSONMap converts a CloudEvent to a JSONMap (resource manifest or status)
func CloudEventToJSONMap(evt *cloudevents.Event) (datatypes.JSONMap, error) {
	evtJSON, err := json.Marshal(evt)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal cloudevent to JSONMAP: %v", err)
	}

	var res datatypes.JSONMap
	if err := res.UnmarshalJSON(evtJSON); err != nil {
		return nil, fmt.Errorf("failed to unmarshal cloudevent JSON to JSONMAP: %v", err)
	}

	return res, nil
}

// EncodeManifest converts a resource manifest (map[string]interface{}) into a CloudEvent JSONMap representation.
func EncodeManifest(manifest map[string]interface{}) (datatypes.JSONMap, error) {
	if len(manifest) == 0 {
		return nil, fmt.Errorf("manifest is empty")
	}

	evt := cetypes.NewEventBuilder("maestro", cetypes.CloudEventsType{}).NewEvent()
	eventPayload := &workpayload.Manifest{
		Manifest: unstructured.Unstructured{Object: manifest},
		DeleteOption: &workv1.DeleteOption{
			PropagationPolicy: workv1.DeletePropagationPolicyTypeForeground,
		},
		ConfigOption: &workpayload.ManifestConfigOption{
			FeedbackRules: []workv1.FeedbackRule{
				{
					Type: workv1.JSONPathsType,
					JsonPaths: []workv1.JsonPath{
						{
							Name: "status",
							Path: ".status",
						},
					},
				},
			},
			UpdateStrategy: &workv1.UpdateStrategy{
				// TODO support external configuration, e.g. configure this through manifest annotations
				Type: workv1.UpdateStrategyTypeServerSideApply,
			},
		},
	}

	if err := evt.SetData(cloudevents.ApplicationJSON, eventPayload); err != nil {
		return nil, fmt.Errorf("failed to set cloud event data: %v", err)
	}

	// convert cloudevent to JSONMap
	manifest, err := CloudEventToJSONMap(&evt)
	if err != nil {
		return nil, fmt.Errorf("failed to convert cloudevent to resource manifest JSON: %v", err)
	}

	return manifest, nil
}

// DecodeManifest converts a CloudEvent JSONMap representation of a resource manifest
// into resource manifest (map[string]interface{}).
func DecodeManifest(manifest datatypes.JSONMap) (map[string]interface{}, error) {
	if len(manifest) == 0 {
		return nil, nil
	}

	evt, err := JSONMAPToCloudEvent(manifest)
	if err != nil {
		return nil, fmt.Errorf("failed to convert resource manifest to cloudevent: %v", err)
	}

	eventPayload := &workpayload.Manifest{}
	if err := evt.DataAs(eventPayload); err != nil {
		return nil, fmt.Errorf("failed to decode cloudevent payload as resource manifest: %v", err)
	}

	return eventPayload.Manifest.Object, nil
}

// DecodeManifestBundle converts a CloudEvent JSONMap representation of a list of resource manifest
// into a list of resource manifest (map[string]interface{}).
func DecodeManifestBundle(manifest datatypes.JSONMap) ([]map[string]interface{}, error) {
	if len(manifest) == 0 {
		return nil, nil
	}

	evt, err := JSONMAPToCloudEvent(manifest)
	if err != nil {
		return nil, fmt.Errorf("failed to convert resource manifest to cloudevent: %v", err)
	}

	eventPayload := &workpayload.ManifestBundle{}
	if err := evt.DataAs(eventPayload); err != nil {
		return nil, fmt.Errorf("failed to decode cloudevent payload as resource manifest bundle: %v", err)
	}

	manifests := make([]map[string]interface{}, 0, len(eventPayload.Manifests))
	for _, m := range eventPayload.Manifests {
		if len(m.Raw) == 0 {
			return nil, fmt.Errorf("manifest in bundle is empty")
		}
		// unmarshal the raw JSON into the object
		obj := &map[string]interface{}{}
		if err := json.Unmarshal(m.Raw, obj); err != nil {
			return nil, fmt.Errorf("failed to unmarshal manifest in bundle: %v", err)
		}
		manifests = append(manifests, *obj)
	}

	return manifests, nil
}

// DecodeManifest converts a CloudEvent JSONMap representation of a resource status
// into resource status (map[string]interface{}).
func DecodeStatus(status datatypes.JSONMap) (map[string]interface{}, error) {
	if len(status) == 0 {
		return nil, nil
	}

	evt, err := JSONMAPToCloudEvent(status)
	if err != nil {
		return nil, fmt.Errorf("failed to convert resource status to cloudevent: %v", err)
	}

	evtExtensions := evt.Extensions()
	resourceVersionInt := int64(0)
	resourceVersion, err := cloudeventstypes.ToString(evtExtensions[cetypes.ExtensionResourceVersion])
	if err != nil {
		resourceVersionIntVal, err := cloudeventstypes.ToInteger(evtExtensions[cetypes.ExtensionResourceVersion])
		if err != nil {
			return nil, fmt.Errorf("failed to get resourceversion extension: %v", err)
		}
		resourceVersionInt = int64(resourceVersionIntVal)
	} else {
		resourceVersionInt, err = strconv.ParseInt(resourceVersion, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to convert resourceversion - %v to int64", resourceVersion)
		}
	}

	sequenceID, err := cloudeventstypes.ToString(evtExtensions[cetypes.ExtensionStatusUpdateSequenceID])
	if err != nil {
		return nil, fmt.Errorf("failed to get sequenceid extension: %v", err)
	}

	resourceStatus := &ResourceStatus{
		ReconcileStatus: &ReconcileStatus{
			ObservedVersion: int32(resourceVersionInt),
			SequenceID:      sequenceID,
		},
	}

	eventPayload := &workpayload.ManifestStatus{}
	if err := evt.DataAs(eventPayload); err != nil {
		return nil, fmt.Errorf("failed to decode cloudevent data as resource status: %v", err)
	}

	if eventPayload.Status != nil {
		resourceStatus.ReconcileStatus.Conditions = eventPayload.Status.Conditions
		for _, value := range eventPayload.Status.StatusFeedbacks.Values {
			if value.Name == "status" {
				contentStatus := make(map[string]interface{})
				if err := json.Unmarshal([]byte(*value.Value.JsonRaw), &contentStatus); err != nil {
					return nil, fmt.Errorf("failed to convert status feedback value to content status: %v", err)
				}
				resourceStatus.ContentStatus = contentStatus
			}
		}
	}

	resourceStatusJSON, err := json.Marshal(resourceStatus)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal resource status to JSON: %v", err)
	}
	statusMap := make(map[string]interface{})
	if err := json.Unmarshal(resourceStatusJSON, &statusMap); err != nil {
		return nil, fmt.Errorf("failed to unmarshal resource status JSON to map: %v", err)
	}

	return statusMap, nil
}
