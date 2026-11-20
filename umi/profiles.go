package umi

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

func configKeys() []string {
	keys := make([]string, 0, len(cfgAllowedKeys))
	for key := range cfgAllowedKeys { keys = append(keys, key) }
	return keys
}

func writeProfileSettings(tx *sql.Tx, cfg map[string]string) error {
	for _, key := range configKeys() {
		if _, err := tx.Exec("DELETE FROM settings WHERE k=?", key); err != nil { return err }
	}
	for key, value := range cfg {
		if cfgAllowedKeys[key] {
			if _, err := tx.Exec("INSERT OR REPLACE INTO settings(k,v) VALUES(?,?)", key, value); err != nil { return err }
		}
	}
	return nil
}

func normalizeProfileConfig(cfg map[string]string) map[string]string {
	out := copyStringMap(DefaultSettings)
	for key, value := range cfg {
		if cfgAllowedKeys[key] { out[key] = value }
	}
	if strings.TrimSpace(out["storage_type"]) == "" { out["storage_type"] = "aliyun" }
	if strings.TrimSpace(out["endpoint"]) == "" { out["endpoint"] = SDKDefaults["endpoint"] }
	for _, key := range []string{"bucket", "bucket_umi"} {
		if strings.TrimSpace(out[key]) == "" { out[key] = SDKDefaults[key] }
	}
	return out
}

// ensureInitialConfigProfile migrates the old flat settings into one named profile.
// Legacy Go versions always used Alibaba OSS SDK, so migration preserves that behavior.
func ensureInitialConfigProfile(db *sql.DB) error {
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM config_profiles").Scan(&count); err != nil { return err }
	if count == 0 {
		cfg := copyStringMap(DefaultSettings)
		rows, err := db.Query("SELECT k,v FROM settings")
		if err != nil { return err }
		for rows.Next() {
			var key, value string
			if err := rows.Scan(&key, &value); err == nil && cfgAllowedKeys[key] { cfg[key] = value }
		}
		if err := rows.Err(); err != nil { rows.Close(); return err }
		rows.Close()
		if cfg["storage_type"] == "" { cfg["storage_type"] = "aliyun" }
		cfg = normalizeProfileConfig(cfg)
		data, err := json.Marshal(cfg)
		if err != nil { return err }
		now := NowTS()
		tx, err := db.Begin()
		if err != nil { return err }
		if _, err := tx.Exec("INSERT INTO config_profiles(id,name,config_json,created_at,updated_at) VALUES(?,?,?,?,?)", "default", "默认配置", string(data), now, now); err != nil { _ = tx.Rollback(); return err }
		if err := writeProfileSettings(tx, cfg); err != nil { _ = tx.Rollback(); return err }
		if _, err := tx.Exec("INSERT OR REPLACE INTO settings(k,v) VALUES('active_profile_id','default')"); err != nil { _ = tx.Rollback(); return err }
		return tx.Commit()
	}
	active := ActiveConfigProfileID()
	var exists int
	if active != "" { _ = db.QueryRow("SELECT COUNT(*) FROM config_profiles WHERE id=?", active).Scan(&exists) }
	if exists == 0 {
		var id, data string
		if err := db.QueryRow("SELECT id,config_json FROM config_profiles ORDER BY created_at,id LIMIT 1").Scan(&id, &data); err != nil { return err }
		var cfg map[string]string
		if err := json.Unmarshal([]byte(data), &cfg); err != nil { return err }
		tx, err := db.Begin()
		if err != nil { return err }
		if err := writeProfileSettings(tx, normalizeProfileConfig(cfg)); err != nil { _ = tx.Rollback(); return err }
		if _, err := tx.Exec("INSERT OR REPLACE INTO settings(k,v) VALUES('active_profile_id',?)", id); err != nil { _ = tx.Rollback(); return err }
		return tx.Commit()
	}
	return nil
}

func ActiveConfigProfileID() string {
	db, err := OpenDB()
	if err != nil { return "" }
	var id string
	_ = db.QueryRow("SELECT v FROM settings WHERE k='active_profile_id'").Scan(&id)
	return id
}

func SaveActiveConfigProfile(cfg map[string]string) error {
	db, err := OpenDB()
	if err != nil { return err }
	id := ActiveConfigProfileID()
	if id == "" { return errors.New("当前配置档案尚未初始化") }
	cfg = normalizeProfileConfig(cfg)
	data, err := json.Marshal(cfg)
	if err != nil { return err }
	tx, err := db.Begin()
	if err != nil { return err }
	if err := writeProfileSettings(tx, cfg); err != nil { _ = tx.Rollback(); return err }
	res, err := tx.Exec("UPDATE config_profiles SET config_json=?,updated_at=? WHERE id=?", string(data), NowTS(), id)
	if err != nil { _ = tx.Rollback(); return err }
	n, _ := res.RowsAffected()
	if n == 0 { _ = tx.Rollback(); return errors.New("当前配置档案不存在") }
	return tx.Commit()
}

func CreateConfigProfile(name string, cfg map[string]string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 40 { return "", errors.New("配置名称不能为空，且不能超过 40 个字符") }
	db, err := OpenDB()
	if err != nil { return "", err }
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil { return "", err }
	id := "profile-" + hex.EncodeToString(raw[:])
	cfg = normalizeProfileConfig(cfg)
	data, err := json.Marshal(cfg)
	if err != nil { return "", err }
	now := NowTS()
	tx, err := db.Begin()
	if err != nil { return "", err }
	if _, err := tx.Exec("INSERT INTO config_profiles(id,name,config_json,created_at,updated_at) VALUES(?,?,?,?,?)", id, name, string(data), now, now); err != nil {
		_ = tx.Rollback()
		return "", fmt.Errorf("创建配置失败（名称可能重复）: %w", err)
	}
	if err := writeProfileSettings(tx, cfg); err != nil { _ = tx.Rollback(); return "", err }
	if _, err := tx.Exec("INSERT OR REPLACE INTO settings(k,v) VALUES('active_profile_id',?)", id); err != nil { _ = tx.Rollback(); return "", err }
	if err := tx.Commit(); err != nil { return "", err }
	return id, nil
}

func ActivateConfigProfile(id string) error {
	db, err := OpenDB()
	if err != nil { return err }
	var data string
	if err := db.QueryRow("SELECT config_json FROM config_profiles WHERE id=?", id).Scan(&data); err != nil { return errors.New("找不到指定配置") }
	var cfg map[string]string
	if err := json.Unmarshal([]byte(data), &cfg); err != nil { return fmt.Errorf("配置档案内容损坏: %w", err) }
	tx, err := db.Begin()
	if err != nil { return err }
	if err := writeProfileSettings(tx, normalizeProfileConfig(cfg)); err != nil { _ = tx.Rollback(); return err }
	if _, err := tx.Exec("INSERT OR REPLACE INTO settings(k,v) VALUES('active_profile_id',?)", id); err != nil { _ = tx.Rollback(); return err }
	return tx.Commit()
}

func DeleteConfigProfile(id string) error {
	db, err := OpenDB()
	if err != nil { return err }
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM config_profiles").Scan(&count); err != nil { return err }
	if count <= 1 { return errors.New("至少保留一套配置，不能删除最后一套") }
	tx, err := db.Begin()
	if err != nil { return err }
	if ActiveConfigProfileID() == id {
		var nextID, data string
		if err := tx.QueryRow("SELECT id,config_json FROM config_profiles WHERE id<>? ORDER BY created_at,id LIMIT 1", id).Scan(&nextID, &data); err != nil { _ = tx.Rollback(); return errors.New("没有可切换的备用配置") }
		var cfg map[string]string
		if err := json.Unmarshal([]byte(data), &cfg); err != nil { _ = tx.Rollback(); return fmt.Errorf("备用配置内容损坏: %w", err) }
		if err := writeProfileSettings(tx, normalizeProfileConfig(cfg)); err != nil { _ = tx.Rollback(); return err }
		if _, err := tx.Exec("INSERT OR REPLACE INTO settings(k,v) VALUES('active_profile_id',?)", nextID); err != nil { _ = tx.Rollback(); return err }
	}
	if _, err := tx.Exec("DELETE FROM config_profiles WHERE id=?", id); err != nil { _ = tx.Rollback(); return err }
	return tx.Commit()
}

func ListConfigProfiles() map[string]any {
	db, err := OpenDB()
	if err != nil { return map[string]any{"profiles": []map[string]any{}, "active_profile_id": ""} }
	activeID := ActiveConfigProfileID()
	rows, err := db.Query("SELECT id,name,config_json FROM config_profiles ORDER BY created_at,id")
	if err != nil { return map[string]any{"profiles": []map[string]any{}, "active_profile_id": activeID} }
	defer rows.Close()
	profiles := make([]map[string]any, 0)
	for rows.Next() {
		var id, name, data string
		if rows.Scan(&id, &name, &data) != nil { continue }
		var cfg map[string]string
		_ = json.Unmarshal([]byte(data), &cfg)
		profiles = append(profiles, map[string]any{
			"id": id, "name": name, "active": id == activeID, "storage_type": cfg["storage_type"],
			"endpoint": cfg["endpoint"], "bucket": cfg["bucket"], "bucket_umi": cfg["bucket_umi"],
		})
	}
	return map[string]any{"profiles": profiles, "active_profile_id": activeID}
}

func mergeConfigValues(base map[string]string, body map[string]any) (map[string]string, string) {
	out := normalizeProfileConfig(base)
	orderProvided := false
	for key, value := range body {
		if !cfgAllowedKeys[key] || value == nil { continue }
		v := strings.TrimSpace(fmt.Sprint(value))
		if key == "sk" && v == "" { continue }
		if key == "order" { orderProvided = true }
		out[key] = v
	}
	if out["storage_type"] == "" { out["storage_type"] = "aliyun" }
	for _, key := range []string{"endpoint", "bucket", "bucket_umi"} {
		if out[key] == "" { out[key] = SDKDefaults[key] }
	}
	if unit := out["ossutil_unit"]; unit != "" && !orderProvided && body["ossutil_unit"] != nil {
		if unit == "session" { out["order"] = "pipeline" } else { out["order"] = "slice" }
	}
	for key, value := range out { if message := cfgCheck(key, value); message != "" { return nil, message } }
	return out, ""
}
