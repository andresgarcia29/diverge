package replay

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
	"pgregory.net/rapid"
)

// TestProperty_JSONComparisonReflexiveAndRemovalBreaking verifies that comparing any generated JSON object
// against itself yields zero diffs, removing a field is always breaking, and adding a field is non-breaking
// unless FailOnExtraFields is enabled.
func TestProperty_JSONComparisonReflexiveAndRemovalBreaking(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		numKeys := rapid.IntRange(2, 6).Draw(t, "numKeys")
		baseMap := make(map[string]any, numKeys)
		keys := make([]string, 0, numKeys)

		for i := 0; i < numKeys; i++ {
			k := fmt.Sprintf("field_%d_%s", i, rapid.StringMatching(`[a-z]{3,8}`).Draw(t, "keySuffix"))
			keys = append(keys, k)
			valKind := rapid.IntRange(0, 3).Draw(t, "valKind")
			switch valKind {
			case 0:
				baseMap[k] = rapid.StringMatching(`[a-zA-Z0-9_-]{1,12}`).Draw(t, "strVal")
			case 1:
				baseMap[k] = rapid.IntRange(1, 10000).Draw(t, "intVal")
			case 2:
				baseMap[k] = rapid.Bool().Draw(t, "boolVal")
			case 3:
				baseMap[k] = map[string]any{
					"nested": rapid.StringMatching(`[a-z]{2,6}`).Draw(t, "nestedVal"),
				}
			}
		}

		baseBytes, err := json.Marshal(baseMap)
		require.NoError(t, err)

		// 1. Reflexivity: identical payloads produce zero diffs even with CompareValues=true
		proto, diffs := ComparePayloads("application/json", "application/json", baseBytes, baseBytes, DiffOptions{CompareValues: true})
		assert.Equal(t, ProtocolJSON, proto)
		assert.Empty(t, diffs)

		// 2. Removing a key is always detected as a breaking missing_field diff
		removedKey := keys[0]
		prevRemoved := make(map[string]any, len(baseMap)-1)
		for k, v := range baseMap {
			if k != removedKey {
				prevRemoved[k] = v
			}
		}
		removedBytes, err := json.Marshal(prevRemoved)
		require.NoError(t, err)

		_, remDiffs := ComparePayloads("application/json", "application/json", baseBytes, removedBytes, DiffOptions{})
		require.NotEmpty(t, remDiffs)
		foundMissing := false
		for _, d := range remDiffs {
			if d.Path == "$."+removedKey && d.Kind == "missing_field" && d.Breaking {
				foundMissing = true
			}
		}
		assert.True(t, foundMissing, "expected breaking missing_field diff for removed key %s", removedKey)

		// 3. Adding a new key is non-breaking by default, and breaking when FailOnExtraFields=true
		prevExtra := make(map[string]any, len(baseMap)+1)
		for k, v := range baseMap {
			prevExtra[k] = v
		}
		prevExtra["extra_added_key"] = "new_val"
		extraBytes, err := json.Marshal(prevExtra)
		require.NoError(t, err)

		_, extraDefaultDiffs := ComparePayloads("application/json", "application/json", baseBytes, extraBytes, DiffOptions{FailOnExtraFields: false})
		require.Len(t, extraDefaultDiffs, 1)
		assert.Equal(t, "$.extra_added_key", extraDefaultDiffs[0].Path)
		assert.Equal(t, "extra_field", extraDefaultDiffs[0].Kind)
		assert.False(t, extraDefaultDiffs[0].Breaking)

		_, extraStrictDiffs := ComparePayloads("application/json", "application/json", baseBytes, extraBytes, DiffOptions{FailOnExtraFields: true})
		require.Len(t, extraStrictDiffs, 1)
		assert.True(t, extraStrictDiffs[0].Breaking)
	})
}

// TestProperty_ProtobufWireReflexiveAndTagRemovalBreaking verifies that comparing arbitrary Protobuf
// wire payloads against themselves produces zero diffs, and dropping a field tag is always detected as breaking.
func TestProperty_ProtobufWireReflexiveAndTagRemovalBreaking(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		numFields := rapid.IntRange(2, 6).Draw(t, "numFields")
		var fullWire []byte
		var withoutFirst []byte

		for i := 0; i < numFields; i++ {
			fieldNum := protowire.Number(i + 1)
			wireChoice := rapid.IntRange(0, 3).Draw(t, "wireChoice")

			var encodedField []byte
			switch wireChoice {
			case 0:
				encodedField = protowire.AppendTag(encodedField, fieldNum, protowire.VarintType)
				encodedField = protowire.AppendVarint(encodedField, rapid.Uint64Range(1, 100000).Draw(t, "varintVal"))
			case 1:
				encodedField = protowire.AppendTag(encodedField, fieldNum, protowire.Fixed32Type)
				encodedField = protowire.AppendFixed32(encodedField, rapid.Uint32().Draw(t, "fixed32Val"))
			case 2:
				encodedField = protowire.AppendTag(encodedField, fieldNum, protowire.Fixed64Type)
				encodedField = protowire.AppendFixed64(encodedField, rapid.Uint64().Draw(t, "fixed64Val"))
			case 3:
				encodedField = protowire.AppendTag(encodedField, fieldNum, protowire.BytesType)
				encodedField = protowire.AppendString(encodedField, rapid.StringMatching(`[a-zA-Z0-9]{1,10}`).Draw(t, "bytesVal"))
			}

			fullWire = append(fullWire, encodedField...)
			if i > 0 {
				withoutFirst = append(withoutFirst, encodedField...)
			}
		}

		// Reflexivity on identical wire bytes
		proto, diffs := ComparePayloads("application/proto", "application/proto", fullWire, fullWire, DiffOptions{CompareValues: true})
		assert.Equal(t, ProtocolProtobuf, proto)
		assert.Empty(t, diffs)

		// Dropping field 1 in preview reports a breaking missing_field diff
		_, dropDiffs := ComparePayloads("application/proto", "application/proto", fullWire, withoutFirst, DiffOptions{})
		require.Len(t, dropDiffs, 1)
		assert.Equal(t, "proto.field_1", dropDiffs[0].Path)
		assert.Equal(t, "missing_field", dropDiffs[0].Kind)
		assert.True(t, dropDiffs[0].Breaking)
	})
}

// TestProperty_StripDivergeBaggagePreservesNonDivergeEntries verifies that stripDivergeBaggage removes
// all diverge.env=<val> members while preserving all other W3C baggage members in order.
func TestProperty_StripDivergeBaggagePreservesNonDivergeEntries(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		numKeep := rapid.IntRange(0, 5).Draw(t, "numKeep")
		var allParts []string
		var expectedKeep []string

		for i := 0; i < numKeep; i++ {
			k := fmt.Sprintf("custom_%d", i)
			v := rapid.StringMatching(`[a-z0-9]{1,8}`).Draw(t, "baggageVal")
			entry := k + "=" + v
			expectedKeep = append(expectedKeep, entry)
			allParts = append(allParts, entry)
			if rapid.Bool().Draw(t, "insertDivergeAfter") {
				allParts = append(allParts, "diverge.env="+rapid.StringMatching(`pr-[0-9]{1,4}`).Draw(t, "envVal"))
			}
		}
		if len(allParts) == 0 {
			allParts = append(allParts, "diverge.env=pr-42")
		}

		hdr := make(http.Header)
		hdr.Set("baggage", strings.Join(allParts, ", "))
		stripDivergeBaggage(&hdr)

		if len(expectedKeep) == 0 {
			assert.Empty(t, hdr.Get("baggage"))
		} else {
			assert.Equal(t, strings.Join(expectedKeep, ","), hdr.Get("baggage"))
		}
	})
}

// TestProperty_StripDivergeCookiePreservesNonDivergeCookies verifies that stripDivergeCookie removes
// all diverge_env=<val> cookies while preserving all other cookies in order.
func TestProperty_StripDivergeCookiePreservesNonDivergeCookies(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		numKeep := rapid.IntRange(0, 5).Draw(t, "numKeep")
		var allCookies []string
		var expectedKeep []string

		for i := 0; i < numKeep; i++ {
			k := fmt.Sprintf("session_%d", i)
			v := rapid.StringMatching(`[a-z0-9]{1,8}`).Draw(t, "cookieVal")
			entry := k + "=" + v
			expectedKeep = append(expectedKeep, entry)
			allCookies = append(allCookies, entry)
			if rapid.Bool().Draw(t, "insertDivergeCookieAfter") {
				allCookies = append(allCookies, "diverge_env="+rapid.StringMatching(`pr-[0-9]{1,4}`).Draw(t, "envCookieVal"))
			}
		}
		if len(allCookies) == 0 {
			allCookies = append(allCookies, "diverge_env=pr-42")
		}

		hdr := make(http.Header)
		hdr.Set("Cookie", strings.Join(allCookies, "; "))
		stripDivergeCookie(&hdr)

		if len(expectedKeep) == 0 {
			assert.Empty(t, hdr.Get("Cookie"))
		} else {
			assert.Equal(t, strings.Join(expectedKeep, "; "), hdr.Get("Cookie"))
		}
	})
}

// TestProperty_PercentileInt64Monotonic verifies that percentileInt64 maintains monotonic ordering
// (0 <= p50 <= p95) for any non-empty slice of non-negative latency samples.
func TestProperty_PercentileInt64Monotonic(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 100).Draw(t, "numSamples")
		samples := make([]int64, n)
		var minVal, maxVal int64
		for i := 0; i < n; i++ {
			v := rapid.Int64Range(0, 60000).Draw(t, "sampleMs")
			samples[i] = v
			if i == 0 || v < minVal {
				minVal = v
			}
			if i == 0 || v > maxVal {
				maxVal = v
			}
		}

		p50 := percentileInt64(samples, 0.50)
		p95 := percentileInt64(samples, 0.95)
		assert.True(t, minVal <= p50, "expected minVal (%d) <= p50 (%d)", minVal, p50)
		assert.True(t, p50 <= p95, "expected p50 (%d) <= p95 (%d)", p50, p95)
		assert.True(t, p95 <= maxVal, "expected p95 (%d) <= maxVal (%d)", p95, maxVal)
	})
}
