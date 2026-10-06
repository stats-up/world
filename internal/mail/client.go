package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"world/internal/docker"
	"world/internal/proxy"
)

// Client habla con la API de administración de Stalwart v0.16: JMAP en POST /jmap
// con los métodos x:<Objeto>/get|set|query (la antigua API REST /api ya no existe).
// Se autentica con la credencial de recuperación (admin) que world le pasa al contenedor.
//
// world corre en el host (fuera de Docker), así que llega a Stalwart por la IP del
// contenedor en la red world; el puerto 8080 nunca se publica.
type Client struct {
	endpoint func(context.Context) (string, error) // URL base del HTTP interno de Stalwart
	secret   func() string
	http     *http.Client

	mu        sync.Mutex
	accountID string
}

func NewClient(dc *docker.Client, adminSecret func() string) *Client {
	return &Client{endpoint: containerEndpoint(dc), secret: adminSecret, http: &http.Client{Timeout: 30 * time.Second}}
}

// ErrNotRunning: el correo está desactivado o el contenedor no corre.
var ErrNotRunning = errors.New("Stalwart no está corriendo")

// MethodError es un error de JMAP a nivel de método o de objeto (notCreated, notUpdated...).
type MethodError struct {
	Type        string `json:"type"`
	Description string `json:"description"`
	Properties  []string
}

func (e *MethodError) Error() string {
	msg := e.Type
	if e.Description != "" {
		msg = e.Description
	}
	if len(e.Properties) > 0 {
		msg += " (" + strings.Join(e.Properties, ", ") + ")"
	}
	return "Stalwart: " + msg
}

// containerEndpoint busca la IP del contenedor en cada llamada: cambia cada vez que se recrea.
func containerEndpoint(dc *docker.Client) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		info, err := dc.ContainerInspect(ctx, ContainerName)
		if docker.IsNotFound(err) {
			return "", ErrNotRunning
		}
		if err != nil {
			return "", err
		}
		ip := info.NetworkSettings.Networks[proxy.Network].IPAddress
		if !info.State.Running || ip == "" {
			return "", ErrNotRunning
		}
		return "http://" + ip + ":8080", nil
	}
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	base, err := c.endpoint(ctx)
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return err
	}
	req.SetBasicAuth(AdminUser, c.secret())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return errors.New("Stalwart rechazó la credencial de administrador (revisa Ajustes → Correo)")
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("Stalwart: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return json.Unmarshal(data, out)
}

// account devuelve el accountId del administrador (se obtiene una vez desde /jmap/session).
func (c *Client) account(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.accountID != "" {
		return c.accountID, nil
	}
	var sess struct {
		Accounts        map[string]json.RawMessage `json:"accounts"`
		PrimaryAccounts map[string]string          `json:"primaryAccounts"`
	}
	if err := c.do(ctx, "GET", "/jmap/session", nil, &sess); err != nil {
		return "", err
	}
	id := sess.PrimaryAccounts["urn:stalwart:jmap"]
	if id == "" {
		id = sess.PrimaryAccounts["urn:ietf:params:jmap:core"]
	}
	if id == "" {
		ids := make([]string, 0, len(sess.Accounts))
		for k := range sess.Accounts {
			ids = append(ids, k)
		}
		sort.Strings(ids)
		if len(ids) == 0 {
			return "", errors.New("Stalwart: la sesión no tiene cuentas")
		}
		id = ids[0]
	}
	c.accountID = id
	return id, nil
}

// call ejecuta un solo método JMAP y decodifica su respuesta en out.
func (c *Client) call(ctx context.Context, method string, args map[string]any, out any) error {
	acc, err := c.account(ctx)
	if err != nil {
		return err
	}
	args["accountId"] = acc
	req := map[string]any{
		"using":       []string{"urn:ietf:params:jmap:core", "urn:stalwart:jmap"},
		"methodCalls": [][]any{{method, args, "0"}},
	}
	var resp struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := c.do(ctx, "POST", "/jmap", req, &resp); err != nil {
		return err
	}
	if len(resp.MethodResponses) == 0 || len(resp.MethodResponses[0]) < 2 {
		return errors.New("Stalwart: respuesta vacía")
	}
	var name string
	json.Unmarshal(resp.MethodResponses[0][0], &name)
	if name == "error" {
		var me MethodError
		json.Unmarshal(resp.MethodResponses[0][1], &me)
		return &me
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(resp.MethodResponses[0][1], out)
}

// setResult es la respuesta de x:<Objeto>/set.
type setResult struct {
	Created      map[string]struct{ ID string } `json:"created"`
	NotCreated   map[string]*MethodError        `json:"notCreated"`
	NotUpdated   map[string]*MethodError        `json:"notUpdated"`
	NotDestroyed map[string]*MethodError        `json:"notDestroyed"`
}

func (r *setResult) err() error {
	for _, m := range []map[string]*MethodError{r.NotCreated, r.NotUpdated, r.NotDestroyed} {
		for _, e := range m {
			return e
		}
	}
	return nil
}

func (c *Client) set(ctx context.Context, object string, args map[string]any) (*setResult, error) {
	var res setResult
	if err := c.call(ctx, "x:"+object+"/set", args, &res); err != nil {
		return nil, err
	}
	return &res, res.err()
}

func (c *Client) create(ctx context.Context, object string, value map[string]any) (string, error) {
	res, err := c.set(ctx, object, map[string]any{"create": map[string]any{"n": value}})
	if err != nil {
		return "", err
	}
	return res.Created["n"].ID, nil
}

func (c *Client) update(ctx context.Context, object, id string, patch map[string]any) error {
	_, err := c.set(ctx, object, map[string]any{"update": map[string]any{id: patch}})
	return err
}

func (c *Client) destroy(ctx context.Context, object, id string) error {
	_, err := c.set(ctx, object, map[string]any{"destroy": []string{id}})
	return err
}

// get trae objetos (ids nil = todos) en out, que debe ser un puntero a slice.
func (c *Client) get(ctx context.Context, object string, ids []string, properties []string, out any) error {
	args := map[string]any{"ids": ids}
	if properties != nil {
		args["properties"] = properties
	}
	var res struct {
		List json.RawMessage `json:"list"`
	}
	if err := c.call(ctx, "x:"+object+"/get", args, &res); err != nil {
		return err
	}
	return json.Unmarshal(res.List, out)
}

func (c *Client) query(ctx context.Context, object string, filter map[string]any) ([]string, error) {
	args := map[string]any{}
	if filter != nil {
		args["filter"] = filter
	}
	var res struct {
		IDs []string `json:"ids"`
	}
	return res.IDs, c.call(ctx, "x:"+object+"/query", args, &res)
}
