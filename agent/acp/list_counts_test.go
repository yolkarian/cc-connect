package acp

import (
	"encoding/json"
	"testing"
)

func TestConvertSessionList_NativeMessageCount(t *testing.T) {
	for _, tc := range []struct {
		name, extra string
		want        int
	}{
		{"native positive count", `,"_meta":{"messageCount":17,"size":1312149}`, 17},
		{"reported zero", `,"_meta":{"messageCount":0}`, 0},
		{"missing metadata", ``, -1},
		{"missing count", `,"_meta":{"size":1312149}`, -1},
		{"null metadata", `,"_meta":null`, -1},
		{"null count", `,"_meta":{"messageCount":null}`, -1},
		{"negative count", `,"_meta":{"messageCount":-2}`, -1},
		{"string count", `,"_meta":{"messageCount":"17"}`, -1},
		{"fractional count", `,"_meta":{"messageCount":1.5}`, -1},
		{"out of range count", `,"_meta":{"messageCount":1e99}`, -1},
		{"non-object metadata", `,"_meta":[17]`, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reply acpSessionListResult
			data := `{"sessions":[{"sessionId":"same-native-uuid","cwd":"/fixture"` + tc.extra + `}]}`
			if err := json.Unmarshal([]byte(data), &reply); err != nil {
				t.Fatal(err)
			}
			entries := convertSessionList(reply.Sessions, "/fixture")
			if len(entries) != 1 || entries[0].ID != "same-native-uuid" || entries[0].MessageCount != tc.want {
				t.Fatalf("native list count lost or unknown became zero: got=%+v want=%d", entries, tc.want)
			}
		})
	}
}
