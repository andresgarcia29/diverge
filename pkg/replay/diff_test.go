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
