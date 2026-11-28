package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLoginEventsAndAlerts(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	uid, _ := st.CreateUser("ana@miempresa.cl", "x")
	add := func(ip, result, sess string) *LoginEvent {
		e := &LoginEvent{UserID: uid, Email: "ana@miempresa.cl", IP: ip, IPKey: ip, Result: result, SessionHash: sess}
		if err := st.AddLoginEvent(e); err != nil {
			t.Fatal(err)
		}
		return e
	}
	if first := add("203.0.113.10", LoginOK, "s1"); first.NewIP {
		t.Error("el primer acceso de todos no es una IP nueva")
	}
	if again := add("203.0.113.10", LoginOK, "s2"); again.NewIP {
		t.Error("IP conocida")
	}
	if !st.KnownLoginIP(uid, "203.0.113.10") || st.KnownLoginIP(uid, "198.51.100.20") {
		t.Error("KnownLoginIP")
	}
	add("198.51.100.20", LoginBadPassword, "")
	other := add("198.51.100.20", LoginOK, "s3")
	if !other.NewIP {
		t.Error("debió marcarse IP nueva")
	}
	add("198.51.100.77", LoginBad2FA, "pend")

	// Desde la sesión nueva no se avisa de sí misma; desde la antigua se ven las dos alertas.
	if a, _ := st.LoginAlerts(uid, "s3"); len(a) != 1 || a[0].Result != LoginBad2FA {
		t.Errorf("desde s3: %+v", a)
	}
	a, _ := st.LoginAlerts(uid, "s1")
	if len(a) != 2 {
		t.Fatalf("desde s1: %d alertas", len(a))
	}
	st.AckLoginAlert(uid+1, a[0].ID) // de otra cuenta: no cambia nada
	if a2, _ := st.LoginAlerts(uid, "s1"); len(a2) != 2 {
		t.Error("ack ajeno aplicado")
	}
	st.AckAllLoginAlerts(uid)
	if a3, _ := st.LoginAlerts(uid, "s1"); len(a3) != 0 {
		t.Error("ack")
	}
	if list, _ := st.LoginEvents(10); len(list) != 5 || list[0].Result != LoginBad2FA {
		t.Errorf("historial: %d", len(list))
	}
	if err := st.PurgeLoginEvents(); err != nil {
		t.Fatal(err)
	}
}

func TestUserSessions(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a, _ := st.CreateUser("ana@miempresa.cl", "x")
	b, _ := st.CreateUser("luis@miempresa.cl", "x")
	exp := time.Now().Add(time.Hour)
	st.CreateSession("h1", a, "c", false, exp, "203.0.113.10", "Mozilla/5.0 (Windows NT 10.0) Chrome/130")
	st.CreateSession("h2", a, "c", true, exp, "198.51.100.20", "")
	st.CreateSession("h3", b, "c", false, exp, "198.51.100.77", "")
	st.CreateSession("old", a, "c", false, time.Now().Add(-time.Minute), "", "")
	st.TouchSession("h2", time.Now().Add(time.Minute))
	list, err := st.UserSessions(a)
	if err != nil || len(list) != 2 || list[0].Hash != "h2" || !list[0].MFAPending || list[1].IP != "203.0.113.10" {
		t.Fatalf("%+v %v", list, err)
	}
	if ok, _ := st.DeleteUserSession(a, "h3"); ok {
		t.Error("cerró una sesión de otra cuenta")
	}
	if ok, _ := st.DeleteUserSession(a, "h1"); !ok {
		t.Error("no cerró la sesión")
	}
}
