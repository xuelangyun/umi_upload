package umi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestConfigProfilesCreateSwitchDelete(t *testing.T) {
	oldPath := DBPath
	CloseDB()
	DBPath = filepath.Join(t.TempDir(), "profiles.db")
	defer func() { CloseDB(); DBPath = oldPath }()
	if err := InitDB(); err != nil { t.Fatal(err) }
	if got := GetCfg()["storage_type"]; got != "aliyun" { t.Fatalf("default storage type = %q, want aliyun", got) }
	defaultID := ActiveConfigProfileID()
	if defaultID == "" { t.Fatal("default profile was not initialized") }
	cfg := GetCfg()
	cfg["storage_type"] = "s3"
	cfg["endpoint"] = "http://127.0.0.1:9000"
	id, err := CreateConfigProfile("测试 MinIO", cfg)
	if err != nil { t.Fatal(err) }
	if GetCfg()["storage_type"] != "s3" { t.Fatal("new profile should be active") }
	if err := ActivateConfigProfile(defaultID); err != nil { t.Fatal(err) }
	if GetCfg()["storage_type"] != "aliyun" { t.Fatal("switch should restore Alibaba profile") }
	if err := ActivateConfigProfile(id); err != nil { t.Fatal(err) }
	if err := DeleteConfigProfile(id); err != nil { t.Fatal(err) }
	if ActiveConfigProfileID() != defaultID || GetCfg()["storage_type"] != "aliyun" { t.Fatal("deleting active profile should switch back to remaining profile") }
	if len(ListConfigProfiles()["profiles"].([]map[string]any)) != 1 { t.Fatal("expected one remaining profile") }
}

func TestLegacyFlatSettingsMigrateToAliyunProfile(t *testing.T) {
	oldPath := DBPath
	CloseDB()
	DBPath = filepath.Join(t.TempDir(), "legacy.db")
	defer func() { CloseDB(); DBPath = oldPath }()
	db, err := OpenDB()
	if err != nil { t.Fatal(err) }
	if _, err := db.Exec("CREATE TABLE settings(k TEXT PRIMARY KEY,v TEXT); INSERT INTO settings(k,v) VALUES('endpoint','https://oss-cn-shenzhen.aliyuncs.com'),('ak','legacy-ak'),('sk','legacy-sk'),('bucket','old-bucket')"); err != nil { t.Fatal(err) }
	if _, err := db.Exec("PRAGMA user_version=6"); err != nil { t.Fatal(err) }
	if err := InitDB(); err != nil { t.Fatal(err) }
	cfg := GetCfg()
	if cfg["storage_type"] != "aliyun" || cfg["endpoint"] != "https://oss-cn-shenzhen.aliyuncs.com" || cfg["ak"] != "legacy-ak" || cfg["sk"] != "legacy-sk" || cfg["bucket"] != "old-bucket" {
		t.Fatalf("legacy settings not preserved: %#v", cfg)
	}
	if ActiveConfigProfileID() != "default" { t.Fatal("legacy config should migrate to default profile") }
}

func TestInitDBRejectsUnsupportedVersionWithoutDroppingData(t *testing.T) {
	oldPath := DBPath
	CloseDB()
	DBPath = filepath.Join(t.TempDir(), "unsupported.db")
	defer func() {
		CloseDB()
		DBPath = oldPath
	}()

	db, err := OpenDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE keep_me (value TEXT NOT NULL); INSERT INTO keep_me(value) VALUES('preserve-this-row'); PRAGMA user_version=999"); err != nil {
		t.Fatal(err)
	}

	if err := InitDB(); err == nil {
		t.Fatal("InitDB should reject an unsupported schema version")
	}
	var value string
	if err := db.QueryRow("SELECT value FROM keep_me").Scan(&value); err != nil {
		t.Fatalf("unsupported database was modified: %v", err)
	}
	if value != "preserve-this-row" {
		t.Fatalf("preserved row = %q, want preserve-this-row", value)
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 999 {
		t.Fatalf("user_version = %d, want 999", version)
	}
}

func TestConfigMutationsSerializeWithTaskAdmission(t *testing.T) {
	oldPath := DBPath
	oldCfg := ActiveCfg()
	CloseDB()
	DBPath = filepath.Join(t.TempDir(), "config-lock.db")
	defer func() {
		CloseDB()
		DBPath = oldPath
		ApplyActiveRuntimeConfig(oldCfg, true)
	}()
	if err := InitDB(); err != nil {
		t.Fatal(err)
	}
	ApplyCfg()

	handler := NewHandler(fstest.MapFS{})
	requestWhileLocked := func(path, body string) *httptest.ResponseRecorder {
		t.Helper()
		StartMu.Lock()
		completed := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			handler.ServeHTTP(recorder, request)
			completed <- recorder
		}()

		select {
		case recorder := <-completed:
			StartMu.Unlock()
			t.Fatalf("%s completed while StartMu was held: status %d, body %s", path, recorder.Code, recorder.Body.String())
		case <-time.After(30 * time.Millisecond):
		}
		StartMu.Unlock()

		select {
		case recorder := <-completed:
			return recorder
		case <-time.After(3 * time.Second):
			t.Fatalf("%s did not complete after StartMu was released", path)
			return nil
		}
	}

	configResponse := requestWhileLocked("/api/config", `{"parallel":"5"}`)
	if configResponse.Code != http.StatusOK {
		t.Fatalf("config status = %d: %s", configResponse.Code, configResponse.Body.String())
	}
	if got := GetCfg()["parallel"]; got != "5" {
		t.Fatalf("parallel = %q, want 5", got)
	}

	defaultID := ActiveConfigProfileID()
	profileConfig := GetCfg()
	profileConfig["parallel"] = "7"
	if _, err := CreateConfigProfile("alternate", profileConfig); err != nil {
		t.Fatal(err)
	}
	ApplyCfg()

	profileResponse := requestWhileLocked("/api/config/profiles", `{"action":"activate","id":"`+defaultID+`"}`)
	if profileResponse.Code != http.StatusOK {
		t.Fatalf("profile activation status = %d: %s", profileResponse.Code, profileResponse.Body.String())
	}
	if got := GetCfg()["parallel"]; got != "5" {
		t.Fatalf("activated profile parallel = %q, want 5", got)
	}
}
