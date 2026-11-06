package replay

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	divergev1alpha1 "github.com/divergedev/diverge/api/gen/diverge/v1alpha1"
)

func TestComparePayloads_JSONContractAndValueDiffs(t *testing.T) {
	baseJSON := []byte(`{
		"order_id": "ord-100",
		"amount_cents": 2500,
		"user": {
			"id": "u-1",
			"email": "alice@example.com"
		},
		"items": [
			{"sku": "sku-1", "qty": 2}
		]
	}`)

	// Preview drops user.email, changes amount_cents from number to string, and adds trace_id
	prevJSON := []byte(`{
		"order_id": "ord-999",
		"amount_cents": "2500",
		"user": {
			"id": "u-1"
		},
		"items": [
			{"sku": "sku-1", "qty": 2}
		],
		"trace_id": "tr-abc"
	}`)

	protocol, diffs := ComparePayloads("application/json", "application/json", baseJSON, prevJSON, DiffOptions{
		CompareValues:     false,
		FailOnExtraFields: false,
	})
	assert.Equal(t, ProtocolJSON, protocol)
	require.Len(t, diffs, 3)

	diffByPath := make(map[string]ContractDiff)
	for _, d := range diffs {
		diffByPath[d.Path] = d
	}

	assert.Equal(t, "type_mismatch", diffByPath["$.amount_cents"].Kind)
	assert.True(t, diffByPath["$.amount_cents"].Breaking)

	assert.Equal(t, "missing_field", diffByPath["$.user.email"].Kind)
	assert.True(t, diffByPath["$.user.email"].Breaking)

	assert.Equal(t, "extra_field", diffByPath["$.trace_id"].Kind)
	assert.False(t, diffByPath["$.trace_id"].Breaking)

	// With CompareValues=true and IgnoreFields=["order_id", "trace_id"]
	_, valDiffs := ComparePayloads("application/json", "application/json", baseJSON, []byte(`{
		"order_id": "ord-999",
		"amount_cents": 2500,
		"user": {"id": "u-1", "email": "bob@example.com"},
		"items": [{"sku": "sku-1", "qty": 2}]
	}`), DiffOptions{
		CompareValues: true,
		IgnoreFields:  []string{"order_id"},
	})
	require.Len(t, valDiffs, 1)
	assert.Equal(t, "$.user.email", valDiffs[0].Path)
	assert.Equal(t, "value_mismatch", valDiffs[0].Kind)
}

func TestComparePayloads_ProtobufWireLevelAndFramedGRPC(t *testing.T) {
	// Construct baseline raw protobuf message:
	// field 1 (varint) = 42
	// field 2 (bytes/string) = "active"
	// field 3 (nested message: field 1 varint = 100, field 2 fixed32 = 7)
	var nestedBase []byte
	nestedBase = protowire.AppendTag(nestedBase, 1, protowire.VarintType)
	nestedBase = protowire.AppendVarint(nestedBase, 100)
	nestedBase = protowire.AppendTag(nestedBase, 2, protowire.Fixed32Type)
	nestedBase = protowire.AppendFixed32(nestedBase, 7)

	var baseProto []byte
	baseProto = protowire.AppendTag(baseProto, 1, protowire.VarintType)
	baseProto = protowire.AppendVarint(baseProto, 42)
	baseProto = protowire.AppendTag(baseProto, 2, protowire.BytesType)
	baseProto = protowire.AppendString(baseProto, "active")
	baseProto = protowire.AppendTag(baseProto, 3, protowire.BytesType)
	baseProto = protowire.AppendBytes(baseProto, nestedBase)

	// Construct preview protobuf message:
	// field 1 changed wire type from varint to bytes ("42")
	// field 2 missing!
	// field 3 nested message missing field 2 (fixed32)
	// field 4 added (varint = 1)
	var nestedPrev []byte
	nestedPrev = protowire.AppendTag(nestedPrev, 1, protowire.VarintType)
	nestedPrev = protowire.AppendVarint(nestedPrev, 100)

	var prevProto []byte
	prevProto = protowire.AppendTag(prevProto, 1, protowire.BytesType)
	prevProto = protowire.AppendString(prevProto, "42")
	prevProto = protowire.AppendTag(prevProto, 3, protowire.BytesType)
	prevProto = protowire.AppendBytes(prevProto, nestedPrev)
	prevProto = protowire.AppendTag(prevProto, 4, protowire.VarintType)
	prevProto = protowire.AppendVarint(prevProto, 1)

	// Wrap both in 5-byte gRPC length-prefixed envelopes
	frame := func(payload []byte) []byte {
		out := make([]byte, 5+len(payload))
		out[0] = 0 // uncompressed
		binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
		copy(out[5:], payload)
		return out
	}

	protocol, diffs := ComparePayloads(
		"application/grpc+proto",
		"application/grpc+proto",
		frame(baseProto),
		frame(prevProto),
		DiffOptions{},
	)
	assert.Equal(t, ProtocolProtobuf, protocol)

	diffByPath := make(map[string]ContractDiff)
	for _, d := range diffs {
		diffByPath[d.Path] = d
	}

	assert.Equal(t, "wire_type_mismatch", diffByPath["proto.field_1"].Kind)
	assert.True(t, diffByPath["proto.field_1"].Breaking)

	assert.Equal(t, "missing_field", diffByPath["proto.field_2"].Kind)
	assert.True(t, diffByPath["proto.field_2"].Breaking)

	assert.Equal(t, "missing_field", diffByPath["proto.field_3.field_2"].Kind)
	assert.True(t, diffByPath["proto.field_3.field_2"].Breaking)

	assert.Equal(t, "extra_field", diffByPath["proto.field_4"].Kind)
	assert.False(t, diffByPath["proto.field_4"].Breaking)
}

func TestComparePayloads_ProtobufWithDescriptorSchema(t *testing.T) {
	// Build a FileDescriptorSet from the compiled diverge v1alpha1 Environment proto descriptor
	envMsg := &divergev1alpha1.GetEnvironmentResponse{}
	fileDesc := envMsg.ProtoReflect().Descriptor().ParentFile()

	fds := &descriptorpb.FileDescriptorSet{}
	visited := make(map[string]bool)
	var walkFile func(f protoreflect.FileDescriptor)
	walkFile = func(f protoreflect.FileDescriptor) {
		if visited[f.Path()] {
			return
		}
		visited[f.Path()] = true
		imports := f.Imports()
		for i := 0; i < imports.Len(); i++ {
			walkFile(imports.Get(i).FileDescriptor)
		}
		fds.File = append(fds.File, protodesc.ToFileDescriptorProto(f))
	}
	walkFile(fileDesc)

	tmpDir := t.TempDir()
	descPath := filepath.Join(tmpDir, "descriptor.binpb")
	descBytes, err := proto.Marshal(fds)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(descPath, descBytes, 0o644))

	protoFiles, err := LoadDescriptorFiles(descPath)
	require.NoError(t, err)

	baseMsg := &divergev1alpha1.GetEnvironmentResponse{
		Environment: &divergev1alpha1.Environment{
			Name:      "pr-42",
			Namespace: "default",
			Status: &divergev1alpha1.EnvironmentStatus{
				Phase: "Ready",
				Url:   "https://pr-42.preview.example.com",
			},
		},
	}
	// Preview omits status.url
	prevMsg := &divergev1alpha1.GetEnvironmentResponse{
		Environment: &divergev1alpha1.Environment{
			Name:      "pr-42",
			Namespace: "default",
			Status: &divergev1alpha1.EnvironmentStatus{
				Phase: "Ready",
			},
		},
	}

	baseBytes, err := proto.Marshal(baseMsg)
	require.NoError(t, err)
	prevBytes, err := proto.Marshal(prevMsg)
	require.NoError(t, err)

	protocol, diffs := ComparePayloads(
		"application/proto",
		"application/proto",
		baseBytes,
		prevBytes,
		DiffOptions{
			ProtoFiles:   protoFiles,
			ProtoMessage: "diverge.v1alpha1.GetEnvironmentResponse",
		},
	)
	assert.Equal(t, ProtocolProtobufTyped, protocol)
	require.Len(t, diffs, 1)
	assert.Equal(t, "$.environment.status.url", diffs[0].Path)
	assert.Equal(t, "missing_field", diffs[0].Kind)
	assert.True(t, diffs[0].Breaking)
}

func TestComparePayloads_JSONArrayLaterElementAndTrailingData(t *testing.T) {
	// Later array element (index 1) missing a baseline field in structural mode
	baseJSON := []byte(`{"items":[{"id":"1","sku":"a"},{"id":"2","sku":"b"}]}`)
	prevJSON := []byte(`{"items":[{"id":"1","sku":"a"},{"id":"2"}]}`)

	protocol, diffs := ComparePayloads("application/json", "application/json", baseJSON, prevJSON, DiffOptions{
		CompareValues: false,
	})
	assert.Equal(t, ProtocolJSON, protocol)
	require.Len(t, diffs, 1)
	assert.Equal(t, "$.items[1].sku", diffs[0].Path)
	assert.Equal(t, "missing_field", diffs[0].Kind)
	assert.True(t, diffs[0].Breaking)

	// Trailing data in preview JSON response is rejected as decode_error
	_, trailingDiffs := ComparePayloads("application/json", "application/json", []byte(`{"ok":true}`), []byte(`{"ok":true}{"error":true}`), DiffOptions{})
	require.Len(t, trailingDiffs, 1)
	assert.Equal(t, "decode_error", trailingDiffs[0].Kind)
	assert.True(t, trailingDiffs[0].Breaking)
}

func TestComparePayloads_ConnectJSONAndProtoTrailers(t *testing.T) {
	makeFrame := func(flags byte, payload []byte) []byte {
		out := make([]byte, 5+len(payload))
		out[0] = flags
		binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
		copy(out[5:], payload)
		return out
	}

	// 1. Connect streaming JSON envelope with identical messages and {} end-of-stream frame passes
	msgFrame := makeFrame(0x00, []byte(`{"id":"ord-1","status":"paid"}`))
	okTrailer := makeFrame(0x02, []byte(`{}`))
	errTrailer := makeFrame(0x02, []byte(`{"error":{"code":"internal","message":"db timeout"}}`))

	baseStream := append(append([]byte{}, msgFrame...), okTrailer...)
	prevStreamOK := append(append([]byte{}, msgFrame...), okTrailer...)
	prevStreamErr := append(append([]byte{}, msgFrame...), errTrailer...)

	protoOK, diffsOK := ComparePayloads("application/connect+json", "application/connect+json", baseStream, prevStreamOK, DiffOptions{})
	assert.Equal(t, ProtocolJSON, protoOK)
	assert.Empty(t, diffsOK)

	// When preview trailer has an RPC error, verification fails with status_mismatch
	_, diffsErr := ComparePayloads("application/connect+json", "application/connect+json", baseStream, prevStreamErr, DiffOptions{})
	require.Len(t, diffsErr, 1)
	assert.Equal(t, "$.rpc_status", diffsErr[0].Path)
	assert.Equal(t, "status_mismatch", diffsErr[0].Kind)
	assert.True(t, diffsErr[0].Breaking)

	// 2. gRPC-Web binary protobuf with nonzero grpc-status trailer in preview fails
	var protoPayload []byte
	protoPayload = protowire.AppendTag(protoPayload, 1, protowire.VarintType)
	protoPayload = protowire.AppendVarint(protoPayload, 7)

	baseWeb := append(makeFrame(0x00, protoPayload), makeFrame(0x80, []byte("grpc-status: 0\r\n"))...)
	prevWeb := append(makeFrame(0x00, protoPayload), makeFrame(0x80, []byte("grpc-status: 13\r\ngrpc-message: internal\r\n"))...)

	_, webDiffs := ComparePayloads("application/grpc-web+proto", "application/grpc-web+proto", baseWeb, prevWeb, DiffOptions{})
	require.Len(t, webDiffs, 1)
	assert.Equal(t, "proto.rpc_status", webDiffs[0].Path)
	assert.Equal(t, "status_mismatch", webDiffs[0].Kind)
	assert.True(t, webDiffs[0].Breaking)
}

func TestComparePayloads_UTF8ProtoFallbackAndRepeatedNestedWire(t *testing.T) {
	// 1. Valid UTF-8 protobuf wire payload (field 1 string = "abc") with application/octet-stream and empty preview body
	var utf8Proto []byte
	utf8Proto = protowire.AppendTag(utf8Proto, 1, protowire.BytesType)
	utf8Proto = protowire.AppendString(utf8Proto, "abc")

	protocol, diffs := ComparePayloads("application/octet-stream", "application/octet-stream", utf8Proto, nil, DiffOptions{})
	assert.Equal(t, ProtocolProtobuf, protocol)
	require.Len(t, diffs, 1)
	assert.Equal(t, "proto.field_1", diffs[0].Path)
	assert.Equal(t, "missing_field", diffs[0].Kind)

	// 2. Repeated nested wire messages preserve occurrence boundaries
	var sub1, sub2 []byte
	sub1 = protowire.AppendTag(sub1, 1, protowire.VarintType)
	sub1 = protowire.AppendVarint(sub1, 10)
	sub1 = protowire.AppendTag(sub1, 2, protowire.VarintType)
	sub1 = protowire.AppendVarint(sub1, 20)

	sub2 = protowire.AppendTag(sub2, 1, protowire.VarintType)
	sub2 = protowire.AppendVarint(sub2, 10)

	// Baseline has [sub1, sub2]; Preview swaps them [sub2, sub1]
	var baseRepeated, prevRepeated []byte
	baseRepeated = protowire.AppendTag(baseRepeated, 3, protowire.BytesType)
	baseRepeated = protowire.AppendBytes(baseRepeated, sub1)
	baseRepeated = protowire.AppendTag(baseRepeated, 3, protowire.BytesType)
	baseRepeated = protowire.AppendBytes(baseRepeated, sub2)

	prevRepeated = protowire.AppendTag(prevRepeated, 3, protowire.BytesType)
	prevRepeated = protowire.AppendBytes(prevRepeated, sub2)
	prevRepeated = protowire.AppendTag(prevRepeated, 3, protowire.BytesType)
	prevRepeated = protowire.AppendBytes(prevRepeated, sub1)

	_, repDiffs := ComparePayloads("application/proto", "application/proto", baseRepeated, prevRepeated, DiffOptions{CompareValues: true})
	require.NotEmpty(t, repDiffs)
	assert.Equal(t, "proto.field_3[0].field_2", repDiffs[0].Path)
	assert.Equal(t, "missing_field", repDiffs[0].Kind)
}

func TestLoadDescriptorFiles(t *testing.T) {
	// Empty path returns nil, nil
	files, err := LoadDescriptorFiles("   ")
	require.NoError(t, err)
	assert.Nil(t, files)

	// Missing file returns error
	_, err = LoadDescriptorFiles("/nonexistent/descriptors.binpb")
	require.Error(t, err)

	// Valid serialized FileDescriptorSet loads and resolves message descriptors
	fds := &descriptorpb.FileDescriptorSet{}
	visited := make(map[string]bool)
	var walkFile func(f protoreflect.FileDescriptor)
	walkFile = func(f protoreflect.FileDescriptor) {
		if visited[f.Path()] {
			return
		}
		visited[f.Path()] = true
		imports := f.Imports()
		for i := 0; i < imports.Len(); i++ {
			walkFile(imports.Get(i).FileDescriptor)
		}
		fds.File = append(fds.File, protodesc.ToFileDescriptorProto(f))
	}
	walkFile((&divergev1alpha1.Environment{}).ProtoReflect().Descriptor().ParentFile())

	rawFDS, err := proto.Marshal(fds)
	require.NoError(t, err)

	tmpFile := filepath.Join(t.TempDir(), "env.binpb")
	require.NoError(t, os.WriteFile(tmpFile, rawFDS, 0o644))

	loaded, err := LoadDescriptorFiles(tmpFile)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	desc, err := loaded.FindDescriptorByName("diverge.v1alpha1.Environment")
	require.NoError(t, err)
	assert.NotNil(t, desc)
}
