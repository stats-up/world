package dbmigrate

import (
	"encoding/base64"
	"strconv"

	"world/internal/secret"
)

// Conexión de administración a MainDB (la base principal en Lightsail). La contraseña va cifrada.
const (
	keyHost = "maindb_host"
	keyPort = "maindb_port"
	keyUser = "maindb_user"
	keyPass = "maindb_password_enc"
)

type SettingsStore interface {
	Setting(key string) string
	SetSetting(key, value string) error
}

// MainDB devuelve la conexión guardada (Host vacío = no configurada).
func MainDB(st SettingsStore, box *secret.Box) Conn {
	c := Conn{Host: st.Setting(keyHost), User: st.Setting(keyUser), Port: 3306}
	if p, err := strconv.Atoi(st.Setting(keyPort)); err == nil && p > 0 {
		c.Port = p
	}
	if raw, err := base64.StdEncoding.DecodeString(st.Setting(keyPass)); err == nil && len(raw) > 0 {
		c.Password, _ = box.OpenString(raw)
	}
	return c
}

// Configured: hay todo lo necesario para conectarse a MainDB.
func Configured(c Conn) bool { return c.Host != "" && c.User != "" && c.Password != "" }

// SaveMainDB guarda la conexión. Contraseña vacía = conservar la actual.
func SaveMainDB(st SettingsStore, box *secret.Box, c Conn) error {
	for k, v := range map[string]string{keyHost: c.Host, keyPort: strconv.Itoa(c.Port), keyUser: c.User} {
		if err := st.SetSetting(k, v); err != nil {
			return err
		}
	}
	if c.Password == "" {
		return nil
	}
	return st.SetSetting(keyPass, base64.StdEncoding.EncodeToString(box.SealString(c.Password)))
}
