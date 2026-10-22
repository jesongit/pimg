package dbx

import (
	"encoding/json"
	"testing"
)

func TestLogReceiptsDistinguishServerImportFromTelegram(t *testing.T) {
	db := activityDB(t)
	for _, item := range []struct{ key, origin, name string }{
		{"legacy-server", "local-import:7:21", "server.png"},
		{"real-telegram", "123:456:7", "telegram.png"},
	} {
		body, _ := json.Marshal(map[string]string{"source": "telegram", "telegram_source": item.origin, "orig_name": item.name})
		if err := db.RecordIngestEvent(IngestEvent{SourceKey: item.key, Type: "accepted", Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	rows, total, err := db.QueryLogReceipts(LogQuery{Page: 1, Limit: 50})
	if err != nil || total != 2 {
		t.Fatal(total, err)
	}
	got := map[string]string{}
	for _, row := range rows {
		got[row.Name] = row.Source
	}
	if got["server.png"] != "local_import" || got["telegram.png"] != "telegram" {
		t.Fatalf("incorrect receipt origin: %#v", got)
	}
}
