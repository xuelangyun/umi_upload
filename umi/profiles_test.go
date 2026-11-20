package umi

import (
	"path/filepath"
	"testing"
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
