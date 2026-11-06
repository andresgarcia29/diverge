package replay

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Protocol constants identifying how a response payload was decoded and compared.
const (
	ProtocolJSON          = "json"
	ProtocolProtobuf      = "protobuf"
	ProtocolProtobufTyped = "protobuf+schema"
	ProtocolText          = "text"
	ProtocolEmpty         = "empty"
)

// ContractDiff describes a single structural, wire-type, or value discrepancy between baseline and preview responses.
type ContractDiff struct {
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	Baseline string `json:"baseline,omitempty"`
	Preview  string `json:"preview,omitempty"`
	Breaking bool   `json:"breaking"`
}

// DiffOptions configures how baseline and preview payloads are compared.
type DiffOptions struct {
	CompareValues     bool
	FailOnExtraFields bool
	IgnoreFields      []string
	ProtoFiles        *protoregistry.Files
	ProtoMessage      string
}

// LoadDescriptorFiles loads a compiled Protobuf FileDescriptorSet (.binpb / .fds) from disk.
func LoadDescriptorFiles(path string) (*protoregistry.Files, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil, nil
	}
	data, err := os.ReadFile(trimmed)
	if err != nil {
		return nil, fmt.Errorf("read proto descriptor %q: %w", trimmed, err)
	}
	var fds descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(data, &fds); err != nil {
		return nil, fmt.Errorf("unmarshal FileDescriptorSet from %q: %w", trimmed, err)
	}
	files, err := protodesc.NewFiles(&fds)
	if err != nil {
		return nil, fmt.Errorf("register FileDescriptorSet from %q: %w", trimmed, err)
	}
	return files, nil
}

// IsProtobufContentType reports whether the HTTP Content-Type header indicates a binary Protobuf, gRPC, or Connect-Proto payload.
func IsProtobufContentType(contentType string) bool {
	mediaType := normalizeMediaType(contentType)
	switch mediaType {
	case "application/proto",
		"application/x-protobuf",
		"application/protobuf",
		"application/vnd.google.protobuf",
		"application/grpc",
		"application/grpc+proto",
		"application/grpc-web",
		"application/grpc-web+proto",
		"application/connect+proto":
		return true
	}
	return strings.HasSuffix(mediaType, "+proto") || strings.HasSuffix(mediaType, "+protobuf")
}

// IsFramedProtoContentType reports whether the Content-Type uses 5-byte length-prefixed framing (gRPC / Connect streaming / gRPC-Web).
func IsFramedProtoContentType(contentType string) bool {
	mediaType := normalizeMediaType(contentType)
	switch mediaType {
	case "application/grpc",
		"application/grpc+proto",
		"application/grpc-web",
		"application/grpc-web+proto",
		"application/connect+proto":
		return true
	}
	return false
}

// IsJSONContentType reports whether the Content-Type header indicates JSON or Connect-JSON.
func IsJSONContentType(contentType string) bool {
	mediaType := normalizeMediaType(contentType)
	if mediaType == "application/json" || mediaType == "text/json" {
		return true
	}
	return strings.HasSuffix(mediaType, "+json")
}

func normalizeMediaType(contentType string) string {
	raw := strings.ToLower(strings.TrimSpace(contentType))
	if idx := strings.IndexByte(raw, ';'); idx >= 0 {
		raw = strings.TrimSpace(raw[:idx])
	}
	return raw
}

// UnwrapProtoEnvelope extracts the raw protobuf message bytes from a gRPC or Connect length-prefixed frame if present.
func UnwrapProtoEnvelope(contentType string, raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	framed := IsFramedProtoContentType(contentType)
	if !framed {
		// Also auto-detect a single uncompressed 5-byte frame when raw[0] == 0 and length matches exactly
		if len(raw) >= 5 && raw[0] == 0 {
			frameLen := int(binary.BigEndian.Uint32(raw[1:5]))
			if frameLen > 0 && frameLen == len(raw)-5 {
				framed = true
			}
		}
	}
	if !framed {
		return raw, nil
	}

	var out bytes.Buffer
	buf := raw
	for len(buf) > 0 {
		if len(buf) < 5 {
			return nil, fmt.Errorf("truncated gRPC/Connect frame header (%d bytes remaining)", len(buf))
		}
		flags := buf[0]
		frameLen := int(binary.BigEndian.Uint32(buf[1:5]))
		buf = buf[5:]
		if frameLen > len(buf) {
			return nil, fmt.Errorf("truncated gRPC/Connect frame payload: expected %d bytes, got %d", frameLen, len(buf))
		}
		framePayload := buf[:frameLen]
		buf = buf[frameLen:]

		// Skip Connect end-of-stream JSON trailer frames (0x02) and gRPC-Web trailer frames (0x80)
		if flags&0x82 != 0 {
			continue
		}
		if flags&0x01 != 0 {
			return nil, fmt.Errorf("compressed gRPC/Connect frame (flags=0x%02x) cannot be decoded without decompression", flags)
		}
		_, _ = out.Write(framePayload)
	}
	return out.Bytes(), nil
}

// ComparePayloads compares baseline and preview HTTP response bodies and returns the detected protocol and contract diffs.
func ComparePayloads(baseContentType, prevContentType string, baseBody, prevBody []byte, opts DiffOptions) (string, []ContractDiff) {
	if len(bytes.TrimSpace(baseBody)) == 0 && len(bytes.TrimSpace(prevBody)) == 0 &&
		!IsProtobufContentType(baseContentType) && !IsProtobufContentType(prevContentType) {
		return ProtocolEmpty, nil
	}

	if IsProtobufContentType(baseContentType) || IsProtobufContentType(prevContentType) ||
		(opts.ProtoMessage != "" && !looksLikeJSON(baseBody) && !looksLikeJSON(prevBody)) {
		return compareProtobufPayloads(baseContentType, prevContentType, baseBody, prevBody, opts)
	}

	if IsJSONContentType(baseContentType) || IsJSONContentType(prevContentType) ||
		(looksLikeJSON(baseBody) && looksLikeJSON(prevBody)) {
		return ProtocolJSON, compareJSONPayloads(baseBody, prevBody, opts)
	}

	// Fallback: if neither JSON nor Protobuf Content-Type was declared, check if both bodies are valid non-empty Protobuf wire messages
	if len(baseBody) > 0 && len(prevBody) > 0 && !utf8.Valid(baseBody) {
		if _, err1 := parseProtoWireMessage(baseBody, 0); err1 == nil {
			if _, err2 := parseProtoWireMessage(prevBody, 0); err2 == nil {
				return compareProtobufPayloads(baseContentType, prevContentType, baseBody, prevBody, opts)
			}
		}
	}

	// Plain text comparison
	var diffs []ContractDiff
	if opts.CompareValues && !bytes.Equal(bytes.TrimSpace(baseBody), bytes.TrimSpace(prevBody)) {
		diffs = append(diffs, ContractDiff{
			Path:     "$",
			Kind:     "value_mismatch",
			Baseline: truncateSample(string(bytes.TrimSpace(baseBody)), 80),
			Preview:  truncateSample(string(bytes.TrimSpace(prevBody)), 80),
			Breaking: true,
		})
	}
	return ProtocolText, diffs
}

func looksLikeJSON(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return false
	}
	first := trimmed[0]
	return first == '{' || first == '['
}

func compareJSONPayloads(baseBody, prevBody []byte, opts DiffOptions) []ContractDiff {
	var baseVal, prevVal any
	baseDec := json.NewDecoder(bytes.NewReader(baseBody))
	baseDec.UseNumber()
	baseErr := baseDec.Decode(&baseVal)

	prevDec := json.NewDecoder(bytes.NewReader(prevBody))
	prevDec.UseNumber()
	prevErr := prevDec.Decode(&prevVal)

	if baseErr != nil || prevErr != nil {
		var diffs []ContractDiff
		if baseErr != nil {
			diffs = append(diffs, ContractDiff{
				Path:     "$",
				Kind:     "decode_error",
				Baseline: fmt.Sprintf("invalid JSON: %v", baseErr),
				Preview:  describeJSONType(prevVal),
				Breaking: true,
			})
		}
		if prevErr != nil {
			diffs = append(diffs, ContractDiff{
				Path:     "$",
				Kind:     "decode_error",
				Baseline: describeJSONType(baseVal),
				Preview:  fmt.Sprintf("invalid JSON: %v", prevErr),
				Breaking: true,
			})
		}
		return diffs
	}

	var diffs []ContractDiff
	diffJSONValues("$", baseVal, prevVal, opts, &diffs)
	return diffs
}

func diffJSONValues(path string, baseVal, prevVal any, opts DiffOptions, diffs *[]ContractDiff) {
	if shouldIgnoreField(path, opts.IgnoreFields) {
		return
	}

	baseType := describeJSONType(baseVal)
	prevType := describeJSONType(prevVal)

	if baseType != prevType {
		*diffs = append(*diffs, ContractDiff{
			Path:     path,
			Kind:     "type_mismatch",
			Baseline: baseType,
			Preview:  prevType,
			Breaking: true,
		})
		return
	}

	switch bTyped := baseVal.(type) {
	case map[string]any:
		pTyped := prevVal.(map[string]any)
		keysMap := make(map[string]struct{}, len(bTyped)+len(pTyped))
		for k := range bTyped {
			keysMap[k] = struct{}{}
		}
		for k := range pTyped {
			keysMap[k] = struct{}{}
		}
		keys := make([]string, 0, len(keysMap))
		for k := range keysMap {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		for _, k := range keys {
			childPath := path + "." + k
			if shouldIgnoreField(childPath, opts.IgnoreFields) {
				continue
			}
			bChild, bHas := bTyped[k]
			pChild, pHas := pTyped[k]
			if bHas && !pHas {
				*diffs = append(*diffs, ContractDiff{
					Path:     childPath,
					Kind:     "missing_field",
					Baseline: describeJSONType(bChild),
					Preview:  "absent",
					Breaking: true,
				})
				continue
			}
			if !bHas && pHas {
				*diffs = append(*diffs, ContractDiff{
					Path:     childPath,
					Kind:     "extra_field",
					Baseline: "absent",
					Preview:  describeJSONType(pChild),
					Breaking: opts.FailOnExtraFields,
				})
				continue
			}
			diffJSONValues(childPath, bChild, pChild, opts, diffs)
		}

	case []any:
		pTyped := prevVal.([]any)
		if opts.CompareValues && len(bTyped) != len(pTyped) {
			*diffs = append(*diffs, ContractDiff{
				Path:     path,
				Kind:     "count_mismatch",
				Baseline: fmt.Sprintf("len=%d", len(bTyped)),
				Preview:  fmt.Sprintf("len=%d", len(pTyped)),
				Breaking: true,
			})
		}
		if len(bTyped) > 0 && len(pTyped) > 0 {
			if opts.CompareValues {
				limit := len(bTyped)
				if len(pTyped) < limit {
					limit = len(pTyped)
				}
				for i := 0; i < limit; i++ {
					diffJSONValues(fmt.Sprintf("%s[%d]", path, i), bTyped[i], pTyped[i], opts, diffs)
				}
			} else {
				// Structural schema comparison checks representative first element
				diffJSONValues(path+"[0]", bTyped[0], pTyped[0], opts, diffs)
			}
		}

	default:
		if opts.CompareValues {
			bStr := formatJSONScalar(baseVal)
			pStr := formatJSONScalar(prevVal)
			if bStr != pStr {
				*diffs = append(*diffs, ContractDiff{
					Path:     path,
					Kind:     "value_mismatch",
					Baseline: truncateSample(bStr, 80),
					Preview:  truncateSample(pStr, 80),
					Breaking: true,
				})
			}
		}
	}
}

func describeJSONType(v any) string {
	if v == nil {
		return "null"
	}
	switch v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case json.Number, float64:
		return "number"
	case bool:
		return "boolean"
	default:
		return fmt.Sprintf("%T", v)
	}
}

func formatJSONScalar(v any) string {
	switch val := v.(type) {
	case nil:
		return "null"
	case string:
		return strconv.Quote(val)
	case json.Number:
		return val.String()
	case bool:
		return strconv.FormatBool(val)
	default:
		return fmt.Sprintf("%v", val)
	}
}

func compareProtobufPayloads(baseContentType, prevContentType string, baseBody, prevBody []byte, opts DiffOptions) (string, []ContractDiff) {
	baseRaw, baseUnwrapErr := UnwrapProtoEnvelope(baseContentType, baseBody)
	prevRaw, prevUnwrapErr := UnwrapProtoEnvelope(prevContentType, prevBody)
	if baseUnwrapErr != nil || prevUnwrapErr != nil {
		var diffs []ContractDiff
		if baseUnwrapErr != nil {
			diffs = append(diffs, ContractDiff{
				Path:     "proto",
				Kind:     "decode_error",
				Baseline: baseUnwrapErr.Error(),
				Preview:  "ok",
				Breaking: true,
			})
		}
		if prevUnwrapErr != nil {
			diffs = append(diffs, ContractDiff{
				Path:     "proto",
				Kind:     "decode_error",
				Baseline: "ok",
				Preview:  prevUnwrapErr.Error(),
				Breaking: true,
			})
		}
		return ProtocolProtobuf, diffs
	}

	// 1. If a FileDescriptorSet and message type are configured, perform schema-aware reflection comparison
	msgName := strings.TrimPrefix(strings.TrimSpace(opts.ProtoMessage), ".")
	if opts.ProtoFiles != nil && msgName != "" {
		desc, err := opts.ProtoFiles.FindDescriptorByName(protoreflect.FullName(msgName))
		if err != nil {
			return ProtocolProtobufTyped, []ContractDiff{{
				Path:     "proto",
				Kind:     "decode_error",
				Baseline: fmt.Sprintf("descriptor %q not found: %v", msgName, err),
				Preview:  "unresolved",
				Breaking: true,
			}}
		}
		msgDesc, ok := desc.(protoreflect.MessageDescriptor)
		if !ok {
			return ProtocolProtobufTyped, []ContractDiff{{
				Path:     "proto",
				Kind:     "decode_error",
				Baseline: fmt.Sprintf("%q is not a protobuf message descriptor", msgName),
				Preview:  "invalid",
				Breaking: true,
			}}
		}
		return ProtocolProtobufTyped, compareDynamicProtoMessages(msgDesc, baseRaw, prevRaw, opts)
	}

	// 2. Zero-config wire-level Protobuf decoding via protowire
	baseWire, baseErr := parseProtoWireMessage(baseRaw, 0)
	prevWire, prevErr := parseProtoWireMessage(prevRaw, 0)
	if baseErr != nil || prevErr != nil {
		var diffs []ContractDiff
		if baseErr != nil {
			diffs = append(diffs, ContractDiff{
				Path:     "proto",
				Kind:     "decode_error",
				Baseline: fmt.Sprintf("invalid protobuf wire payload: %v", baseErr),
				Preview:  "valid",
				Breaking: true,
			})
		}
		if prevErr != nil {
			diffs = append(diffs, ContractDiff{
				Path:     "proto",
				Kind:     "decode_error",
				Baseline: "valid",
				Preview:  fmt.Sprintf("invalid protobuf wire payload: %v", prevErr),
				Breaking: true,
			})
		}
		return ProtocolProtobuf, diffs
	}

	var diffs []ContractDiff
	diffProtoWireMessages("proto", baseWire, prevWire, opts, &diffs)
	return ProtocolProtobuf, diffs
}

func compareDynamicProtoMessages(msgDesc protoreflect.MessageDescriptor, baseRaw, prevRaw []byte, opts DiffOptions) []ContractDiff {
	baseMsg := dynamicpb.NewMessage(msgDesc)
	prevMsg := dynamicpb.NewMessage(msgDesc)

	baseErr := proto.Unmarshal(baseRaw, baseMsg)
	prevErr := proto.Unmarshal(prevRaw, prevMsg)
	if baseErr != nil || prevErr != nil {
		var diffs []ContractDiff
		if baseErr != nil {
			diffs = append(diffs, ContractDiff{
				Path:     "$",
				Kind:     "decode_error",
				Baseline: fmt.Sprintf("proto unmarshal failed: %v", baseErr),
				Preview:  "valid",
				Breaking: true,
			})
		}
		if prevErr != nil {
			diffs = append(diffs, ContractDiff{
				Path:     "$",
				Kind:     "decode_error",
				Baseline: "valid",
				Preview:  fmt.Sprintf("proto unmarshal failed: %v", prevErr),
				Breaking: true,
			})
		}
		return diffs
	}

	var diffs []ContractDiff
	diffReflectedProtoMessages("$", baseMsg.ProtoReflect(), prevMsg.ProtoReflect(), opts, &diffs)
	return diffs
}

func diffReflectedProtoMessages(path string, baseMsg, prevMsg protoreflect.Message, opts DiffOptions, diffs *[]ContractDiff) {
	if shouldIgnoreField(path, opts.IgnoreFields) {
		return
	}

	fields := baseMsg.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		fieldName := string(fd.Name())
		childPath := fmt.Sprintf("%s.%s", path, fieldName)
		tagAlias := fmt.Sprintf("field_%d", fd.Number())
		if shouldIgnoreField(childPath, opts.IgnoreFields) || shouldIgnoreField(tagAlias, opts.IgnoreFields) {
			continue
		}

		bHas := protoFieldPresent(baseMsg, fd)
		pHas := protoFieldPresent(prevMsg, fd)
		fieldTypeDesc := describeProtoFieldDescriptor(fd)

		if bHas && !pHas {
			*diffs = append(*diffs, ContractDiff{
				Path:     childPath,
				Kind:     "missing_field",
				Baseline: fieldTypeDesc,
				Preview:  "absent",
				Breaking: true,
			})
			continue
		}
		if !bHas && pHas {
			*diffs = append(*diffs, ContractDiff{
				Path:     childPath,
				Kind:     "extra_field",
				Baseline: "absent",
				Preview:  fieldTypeDesc,
				Breaking: opts.FailOnExtraFields,
			})
			continue
		}
		if !bHas && !pHas {
			continue
		}

		bVal := baseMsg.Get(fd)
		pVal := prevMsg.Get(fd)

		if fd.IsList() {
			bList := bVal.List()
			pList := pVal.List()
			if opts.CompareValues && bList.Len() != pList.Len() {
				*diffs = append(*diffs, ContractDiff{
					Path:     childPath,
					Kind:     "count_mismatch",
					Baseline: fmt.Sprintf("len=%d", bList.Len()),
					Preview:  fmt.Sprintf("len=%d", pList.Len()),
					Breaking: true,
				})
			}
			if fd.Kind() == protoreflect.MessageKind && bList.Len() > 0 && pList.Len() > 0 {
				if opts.CompareValues {
					limit := bList.Len()
					if pList.Len() < limit {
						limit = pList.Len()
					}
					for idx := 0; idx < limit; idx++ {
						diffReflectedProtoMessages(fmt.Sprintf("%s[%d]", childPath, idx), bList.Get(idx).Message(), pList.Get(idx).Message(), opts, diffs)
					}
				} else {
					diffReflectedProtoMessages(childPath+"[0]", bList.Get(0).Message(), pList.Get(0).Message(), opts, diffs)
				}
			} else if opts.CompareValues {
				limit := bList.Len()
				if pList.Len() < limit {
					limit = pList.Len()
				}
				for idx := 0; idx < limit; idx++ {
					bStr := formatProtoScalar(fd, bList.Get(idx))
					pStr := formatProtoScalar(fd, pList.Get(idx))
					if bStr != pStr {
						*diffs = append(*diffs, ContractDiff{
							Path:     fmt.Sprintf("%s[%d]", childPath, idx),
							Kind:     "value_mismatch",
							Baseline: truncateSample(bStr, 80),
							Preview:  truncateSample(pStr, 80),
							Breaking: true,
						})
					}
				}
			}
			continue
		}

		if fd.IsMap() {
			bMap := bVal.Map()
			pMap := pVal.Map()
			if opts.CompareValues && bMap.Len() != pMap.Len() {
				*diffs = append(*diffs, ContractDiff{
					Path:     childPath,
					Kind:     "count_mismatch",
					Baseline: fmt.Sprintf("len=%d", bMap.Len()),
					Preview:  fmt.Sprintf("len=%d", pMap.Len()),
					Breaking: true,
				})
			}
			continue
		}

		if fd.Kind() == protoreflect.MessageKind {
			diffReflectedProtoMessages(childPath, bVal.Message(), pVal.Message(), opts, diffs)
			continue
		}

		if opts.CompareValues {
			bStr := formatProtoScalar(fd, bVal)
			pStr := formatProtoScalar(fd, pVal)
			if bStr != pStr {
				*diffs = append(*diffs, ContractDiff{
					Path:     childPath,
					Kind:     "value_mismatch",
					Baseline: truncateSample(bStr, 80),
					Preview:  truncateSample(pStr, 80),
					Breaking: true,
				})
			}
		}
	}

	// Also inspect unknown fields via wire parser in case preview or baseline emitted tags not in descriptor
	baseUnknown, _ := parseProtoWireMessage(baseMsg.GetUnknown(), 0)
	prevUnknown, _ := parseProtoWireMessage(prevMsg.GetUnknown(), 0)
	if len(baseUnknown) > 0 || len(prevUnknown) > 0 {
		diffProtoWireMessages(path, baseUnknown, prevUnknown, opts, diffs)
	}
}

func protoFieldPresent(msg protoreflect.Message, fd protoreflect.FieldDescriptor) bool {
	if fd.IsList() {
		return msg.Get(fd).List().Len() > 0
	}
	if fd.IsMap() {
		return msg.Get(fd).Map().Len() > 0
	}
	return msg.Has(fd)
}

func describeProtoFieldDescriptor(fd protoreflect.FieldDescriptor) string {
	kind := fd.Kind().String()
	if fd.Kind() == protoreflect.MessageKind {
		kind = string(fd.Message().FullName())
	} else if fd.Kind() == protoreflect.EnumKind {
		kind = string(fd.Enum().FullName())
	}
	if fd.IsList() {
		return "repeated " + kind
	}
	if fd.IsMap() {
		return "map<" + fd.MapKey().Kind().String() + "," + fd.MapValue().Kind().String() + ">"
	}
	return fmt.Sprintf("%s (#%d)", kind, fd.Number())
}

func formatProtoScalar(fd protoreflect.FieldDescriptor, v protoreflect.Value) string {
	switch fd.Kind() {
	case protoreflect.StringKind:
		return strconv.Quote(v.String())
	case protoreflect.BytesKind:
		return "0x" + hex.EncodeToString(v.Bytes())
	case protoreflect.EnumKind:
		enumVal := fd.Enum().Values().ByNumber(v.Enum())
		if enumVal != nil {
			return string(enumVal.Name())
		}
		return strconv.Itoa(int(v.Enum()))
	default:
		return v.String()
	}
}

type wireFieldSummary struct {
	Number   protowire.Number
	WireType string
	Count    int
	Values   []string
	Nested   map[protowire.Number]*wireFieldSummary
}

func parseProtoWireMessage(b []byte, depth int) (map[protowire.Number]*wireFieldSummary, error) {
	fields := make(map[protowire.Number]*wireFieldSummary)
	for len(b) > 0 {
		num, wtype, tagLen := protowire.ConsumeTag(b)
		if tagLen < 0 {
			return nil, protowire.ParseError(tagLen)
		}
		if num <= 0 {
			return nil, fmt.Errorf("invalid protobuf field number %d", num)
		}
		rest := b[tagLen:]

		var (
			valLen   int
			wtypeStr string
			valRepr  string
			nested   map[protowire.Number]*wireFieldSummary
		)

		switch wtype {
		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(rest)
			if n < 0 {
				return nil, protowire.ParseError(n)
			}
			valLen = n
			wtypeStr = "varint"
			valRepr = strconv.FormatUint(v, 10)

		case protowire.Fixed32Type:
			v, n := protowire.ConsumeFixed32(rest)
			if n < 0 {
				return nil, protowire.ParseError(n)
			}
			valLen = n
			wtypeStr = "fixed32"
			valRepr = fmt.Sprintf("0x%08x", v)

		case protowire.Fixed64Type:
			v, n := protowire.ConsumeFixed64(rest)
			if n < 0 {
				return nil, protowire.ParseError(n)
			}
			valLen = n
			wtypeStr = "fixed64"
			valRepr = fmt.Sprintf("0x%016x", v)

		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(rest)
			if n < 0 {
				return nil, protowire.ParseError(n)
			}
			valLen = n
			wtypeStr = "bytes"
			if depth < 5 && isLikelyNestedProtoMessage(v) {
				if sub, err := parseProtoWireMessage(v, depth+1); err == nil && len(sub) > 0 {
					nested = sub
					wtypeStr = "bytes/message"
				}
			}
			if utf8.Valid(v) && isPrintableUTF8(string(v)) {
				valRepr = strconv.Quote(string(v))
			} else {
				valRepr = "0x" + hex.EncodeToString(v)
			}

		case protowire.StartGroupType:
			v, n := protowire.ConsumeGroup(num, rest)
			if n < 0 {
				return nil, protowire.ParseError(n)
			}
			valLen = n
			wtypeStr = "group"
			valRepr = "0x" + hex.EncodeToString(v)

		default:
			return nil, fmt.Errorf("unsupported protobuf wire type %d for field %d", wtype, num)
		}

		entry, exists := fields[num]
		if !exists {
			entry = &wireFieldSummary{
				Number:   num,
				WireType: wtypeStr,
			}
			fields[num] = entry
		} else if entry.WireType != wtypeStr {
			// If one occurrence looked like a nested message and another was plain bytes, normalize to bytes
			if (entry.WireType == "bytes/message" && wtypeStr == "bytes") || (entry.WireType == "bytes" && wtypeStr == "bytes/message") {
				entry.WireType = "bytes"
				entry.Nested = nil
			} else {
				entry.WireType = entry.WireType + "|" + wtypeStr
			}
		}

		entry.Count++
		entry.Values = append(entry.Values, valRepr)
		if nested != nil && entry.WireType == "bytes/message" {
			if entry.Nested == nil {
				entry.Nested = nested
			} else {
				for k, subVal := range nested {
					if existingSub, ok := entry.Nested[k]; ok {
						existingSub.Count += subVal.Count
						existingSub.Values = append(existingSub.Values, subVal.Values...)
					} else {
						entry.Nested[k] = subVal
					}
				}
			}
		}

		b = rest[valLen:]
	}
	return fields, nil
}

func isLikelyNestedProtoMessage(b []byte) bool {
	if len(b) < 2 {
		return false
	}
	// If the entire slice is printable ASCII text (e.g. "hello world"), treat it as a string rather than a message
	if utf8.Valid(b) && isPrintableUTF8(string(b)) {
		return false
	}
	rem := b
	fieldCount := 0
	for len(rem) > 0 {
		num, wtype, n := protowire.ConsumeField(rem)
		if n < 0 || num <= 0 || num > 19000 {
			return false
		}
		if wtype == protowire.StartGroupType || wtype == protowire.EndGroupType {
			return false
		}
		fieldCount++
		rem = rem[n:]
	}
	return fieldCount > 0
}

func isPrintableUTF8(s string) bool {
	if s == "" {
		return true
	}
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func diffProtoWireMessages(pathPrefix string, baseFields, prevFields map[protowire.Number]*wireFieldSummary, opts DiffOptions, diffs *[]ContractDiff) {
	numsMap := make(map[protowire.Number]struct{}, len(baseFields)+len(prevFields))
	for num := range baseFields {
		numsMap[num] = struct{}{}
	}
	for num := range prevFields {
		numsMap[num] = struct{}{}
	}
	nums := make([]int, 0, len(numsMap))
	for num := range numsMap {
		nums = append(nums, int(num))
	}
	sort.Ints(nums)

	for _, rawNum := range nums {
		num := protowire.Number(rawNum)
		fieldPath := fmt.Sprintf("%s.field_%d", pathPrefix, num)
		if shouldIgnoreField(fieldPath, opts.IgnoreFields) {
			continue
		}

		bField, bHas := baseFields[num]
		pField, pHas := prevFields[num]

		if bHas && !pHas {
			*diffs = append(*diffs, ContractDiff{
				Path:     fieldPath,
				Kind:     "missing_field",
				Baseline: bField.WireType,
				Preview:  "absent",
				Breaking: true,
			})
			continue
		}
		if !bHas && pHas {
			*diffs = append(*diffs, ContractDiff{
				Path:     fieldPath,
				Kind:     "extra_field",
				Baseline: "absent",
				Preview:  pField.WireType,
				Breaking: opts.FailOnExtraFields,
			})
			continue
		}

		if normalizeWireCategory(bField.WireType) != normalizeWireCategory(pField.WireType) {
			*diffs = append(*diffs, ContractDiff{
				Path:     fieldPath,
				Kind:     "wire_type_mismatch",
				Baseline: bField.WireType,
				Preview:  pField.WireType,
				Breaking: true,
			})
			continue
		}

		if bField.Nested != nil && pField.Nested != nil {
			diffProtoWireMessages(fieldPath, bField.Nested, pField.Nested, opts, diffs)
			continue
		}

		if opts.CompareValues {
			if bField.Count != pField.Count {
				*diffs = append(*diffs, ContractDiff{
					Path:     fieldPath,
					Kind:     "count_mismatch",
					Baseline: fmt.Sprintf("count=%d", bField.Count),
					Preview:  fmt.Sprintf("count=%d", pField.Count),
					Breaking: true,
				})
				continue
			}
			bJoined := strings.Join(bField.Values, ",")
			pJoined := strings.Join(pField.Values, ",")
			if bJoined != pJoined {
				*diffs = append(*diffs, ContractDiff{
					Path:     fieldPath,
					Kind:     "value_mismatch",
					Baseline: truncateSample(bJoined, 80),
					Preview:  truncateSample(pJoined, 80),
					Breaking: true,
				})
			}
		}
	}
}

func normalizeWireCategory(w string) string {
	if w == "bytes/message" {
		return "bytes"
	}
	return w
}

func shouldIgnoreField(path string, ignoreList []string) bool {
	if len(ignoreList) == 0 {
		return false
	}
	for _, rawIgnore := range ignoreList {
		ig := strings.TrimSpace(rawIgnore)
		if ig == "" {
			continue
		}
		if path == ig || strings.HasSuffix(path, "."+ig) {
			return true
		}
	}
	return false
}

func truncateSample(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
