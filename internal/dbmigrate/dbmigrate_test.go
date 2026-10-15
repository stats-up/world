package dbmigrate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"world/internal/secret"
)

// Trozo de un dump de MariaDB 11 con lo que MySQL 8 administrado no acepta.
const mariaDump = "-- Table structure for table `pedidos`\n" +
	"CREATE TABLE `pedidos` (\n  `id` int NOT NULL\n) ENGINE=Aria DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_uca1400_ai_ci PAGE_CHECKSUM=1 TRANSACTIONAL=1;\n" +
	"-- Table structure for table `logs`\n" +
	"CREATE TABLE `logs` (`m` text COLLATE utf8mb3_uca1400_as_cs) ENGINE=MyISAM;\n" +
	"/*!50013 DEFINER=`cpuser_admin`@`localhost` SQL SECURITY DEFINER */\n" +
	"/*!50003 CREATE*/ /*!50017 DEFINER=`root`@`%`*/ /*!50003 TRIGGER t1 BEFORE INSERT ON pedidos FOR EACH ROW SET NEW.id = 1 */;;\n" +
	"CREATE DEFINER=`x`@`10.0.0.%` PROCEDURE `p`() SELECT 1;\n"

func TestSedProgram(t *testing.T) {
	sed, err := exec.LookPath("sed")
	if err != nil {
		t.Skip("sin sed")
	}
	cmd := exec.Command(sed, "-E", "-e", sedProgram)
	cmd.Stdin = strings.NewReader(mariaDump)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err, stderr.String())
	}
	got := string(out)
	for _, bad := range []string{"DEFINER=", "uca1400", "Aria", "MyISAM", "PAGE_CHECKSUM", "TRANSACTIONAL"} {
		if strings.Contains(got, bad) {
			t.Errorf("quedó %q:\n%s", bad, got)
		}
	}
	for _, want := range []string{"ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;", "COLLATE utf8mb3_unicode_ci", "CREATE  PROCEDURE `p`()", "SQL SECURITY DEFINER"} {
		if !strings.Contains(got, want) {
			t.Errorf("falta %q:\n%s", want, got)
		}
	}
	// El avance: cada tabla se anota en el log (stderr dentro del contenedor).
	if CountTables(stderr.String()) != 2 {
		t.Errorf("avance: %q", stderr.String())
	}
}

// El archivo de opciones debe conservar contraseñas con comillas, barras y $.
func TestScriptOptionFile(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("sin bash")
	}
	dir := t.TempDir()
	esc := script[strings.Index(script, "esc()"):strings.Index(script, "umask")]
	pass := `p"a\ss$x 'y'`
	prog := esc + `printf '[client]\nuser="%s"\npassword="%s"\n' "$(esc "$U")" "$(esc "$P")" > "$OUT"`
	// Desde un archivo: en Windows, pasar el script por -c altera las barras y comillas.
	file := filepath.Join(dir, "s.sh")
	os.WriteFile(file, []byte(prog), 0o600)
	cmd := exec.Command(bash, file)
	cmd.Env = append(os.Environ(), "U=cp_user", "P="+pass, "OUT="+filepath.Join(dir, "c.cnf"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	b, _ := os.ReadFile(filepath.Join(dir, "c.cnf"))
	if want := "[client]\nuser=\"cp_user\"\npassword=\"p\\\"a\\\\ss$x 'y'\"\n"; string(b) != want {
		t.Fatalf("got %q\nwant %q", b, want)
	}
}

func TestExitMessage(t *testing.T) {
	cases := map[string]string{
		"ERROR 1419 (HY000) at line 120: You do not have the SUPER privilege and binary logging is enabled": "log_bin_trust_function_creators",
		"mysqldump: Got error: 1045: Access denied for user 'x'@'1.2.3.4' (using password: YES) when trying to connect": "Usuario o contraseña",
		"ERROR 1273 (HY000) at line 40: Unknown collation: 'utf8mb4_0900_bin2'":                                       "intercalación",
		"ERROR 2013 (HY000) at line 99: Lost connection to MySQL server during query":                                 "conexión",
	}
	for logText, want := range cases {
		if got := ExitMessage(1, "== inicio\n"+logText+"\n"); !strings.Contains(got, want) {
			t.Errorf("%q → %q (esperaba %q)", logText, got, want)
		}
	}
	if got := ExitMessage(137, "nada"); !strings.Contains(got, "se detuvo") {
		t.Error(got)
	}
}

func TestTargetCharset(t *testing.T) {
	cases := [][4]string{
		{"utf8mb4", "utf8mb4_uca1400_ai_ci", "utf8mb4", "utf8mb4_unicode_ci"},
		{"utf8mb4", "utf8mb4_general_ci", "utf8mb4", "utf8mb4_general_ci"},
		{"utf8", "utf8_general_ci", "utf8mb3", "utf8mb3_general_ci"},
		{"latin1", "latin1_swedish_ci", "latin1", "latin1_swedish_ci"},
		{"", "", "utf8mb4", "utf8mb4_unicode_ci"},
		{"utf8mb4; DROP", "x", "utf8mb4", "utf8mb4_unicode_ci"},
	}
	for _, c := range cases {
		if cs, co := targetCharset(c[0], c[1]); cs != c[2] || co != c[3] {
			t.Errorf("%s/%s → %s/%s", c[0], c[1], cs, co)
		}
	}
}

func TestNamesAndSettings(t *testing.T) {
	for _, ok := range []string{"tienda", "cp_user_tienda2"} {
		if !ValidNewName(ok) {
			t.Error(ok)
		}
	}
	for _, bad := range []string{"", "a-b", "a b", "x`;", strings.Repeat("a", 65)} {
		if ValidNewName(bad) {
			t.Error(bad)
		}
	}
	if !systemDB("MySQL") || systemDB("tienda") {
		t.Error("systemDB")
	}
	if quoteIdent("a`b") != "`a``b`" {
		t.Error(quoteIdent("a`b"))
	}

	box, _ := secret.LoadOrCreate(filepath.Join(t.TempDir(), "k"))
	st := memStore{}
	if Configured(MainDB(st, box)) {
		t.Fatal("sin configurar")
	}
	SaveMainDB(st, box, Conn{Host: "ls-x.rds.amazonaws.com", Port: 3306, User: "dbmasteruser", Password: "s3cr3t"})
	if st[keyPass] == "" || strings.Contains(st[keyPass], "s3cr3t") {
		t.Fatal("la contraseña debe ir cifrada")
	}
	SaveMainDB(st, box, Conn{Host: "ls-y", Port: 3307, User: "admin"}) // vacía = conservar
	if c := MainDB(st, box); c.Host != "ls-y" || c.Port != 3307 || c.Password != "s3cr3t" {
		t.Fatalf("%+v", c)
	}
}

type memStore map[string]string

func (m memStore) Setting(k string) string      { return m[k] }
func (m memStore) SetSetting(k, v string) error { m[k] = v; return nil }
