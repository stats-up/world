package mail

import (
	"encoding/base64"

	"world/internal/secret"
)

// Claves en la tabla settings. Las terminadas en _enc van cifradas con secret.Box (base64).
const (
	keyEnabled     = "mail_enabled"
	keyHostname    = "mail_hostname"
	keyS3Bucket    = "mail_s3_bucket"
	keyS3Prefix    = "mail_s3_prefix"
	keyS3AccessKey = "mail_s3_access_key_enc"
	keyS3SecretKey = "mail_s3_secret_key_enc"
	keyCFToken     = "mail_cf_token_enc"
	keyAdminSecret = "mail_admin_secret_enc"
)

// SettingsStore es lo que se necesita de store.Store (permite probar sin SQLite).
type SettingsStore interface {
	Setting(key string) string
	SetSetting(key, value string) error
}

// Enabled indica si el correo está activado, sin descifrar los secretos.
func Enabled(st SettingsStore) bool { return st.Setting(keyEnabled) == "1" }

// Disable desactiva el correo conservando el resto de los ajustes.
func Disable(st SettingsStore) error { return st.SetSetting(keyEnabled, "0") }

func LoadSettings(st SettingsStore, box *secret.Box) Settings {
	open := func(key string) string {
		raw, err := base64.StdEncoding.DecodeString(st.Setting(key))
		if err != nil || len(raw) == 0 {
			return ""
		}
		v, _ := box.OpenString(raw)
		return v
	}
	s := Settings{
		Enabled:     st.Setting(keyEnabled) == "1",
		Hostname:    st.Setting(keyHostname),
		S3Bucket:    st.Setting(keyS3Bucket),
		S3Prefix:    st.Setting(keyS3Prefix),
		S3AccessKey: open(keyS3AccessKey),
		S3SecretKey: open(keyS3SecretKey),
		CFToken:     open(keyCFToken),
		AdminSecret: open(keyAdminSecret),
	}
	if s.Hostname == "" {
		s.Hostname = "mx.statsup.cl"
	}
	if s.S3Prefix == "" {
		s.S3Prefix = "mail/"
	}
	return s
}

// SaveSettings guarda todo. Los secretos vacíos no se tocan: en el formulario, vacío = "dejar el actual".
func SaveSettings(st SettingsStore, box *secret.Box, s Settings) error {
	enabled := "0"
	if s.Enabled {
		enabled = "1"
	}
	plain := map[string]string{keyEnabled: enabled, keyHostname: s.Hostname, keyS3Bucket: s.S3Bucket, keyS3Prefix: s.S3Prefix}
	for k, v := range plain {
		if err := st.SetSetting(k, v); err != nil {
			return err
		}
	}
	secrets := map[string]string{keyS3AccessKey: s.S3AccessKey, keyS3SecretKey: s.S3SecretKey, keyCFToken: s.CFToken, keyAdminSecret: s.AdminSecret}
	for k, v := range secrets {
		if v == "" {
			continue
		}
		if err := st.SetSetting(k, base64.StdEncoding.EncodeToString(box.SealString(v))); err != nil {
			return err
		}
	}
	return nil
}
