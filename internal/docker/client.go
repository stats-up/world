// Package docker es un cliente mínimo de la Docker Engine API sobre el socket unix.
// Se usa la API HTTP directamente (sin el SDK oficial) para mantener el binario
// pequeño y sin dependencias que cambian de versión a menudo.
package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	http *http.Client
}

func New(socket string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &Client{http: &http.Client{Transport: tr}}
}

type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("docker: %s (HTTP %d)", e.Message, e.Status) }

func IsNotFound(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	u := "http://docker" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var msg struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &msg) != nil || msg.Message == "" {
			msg.Message = strings.TrimSpace(string(data))
		}
		return nil, &APIError{Status: resp.StatusCode, Message: msg.Message}
	}
	return resp, nil
}

func (c *Client) call(ctx context.Context, method, path string, query url.Values, body, out any) error {
	resp, err := c.do(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}

func filters(kv map[string][]string) url.Values {
	b, _ := json.Marshal(kv)
	return url.Values{"filters": {string(b)}}
}

// --- Sistema ---

type Info struct {
	ServerVersion     string `json:"ServerVersion"`
	NCPU              int    `json:"NCPU"`
	MemTotal          int64  `json:"MemTotal"`
	ContainersRunning int    `json:"ContainersRunning"`
	Images            int    `json:"Images"`
}

func (c *Client) Info(ctx context.Context) (*Info, error) {
	var info Info
	return &info, c.call(ctx, "GET", "/info", nil, nil, &info)
}

// --- Contenedores ---

type ContainerConfig struct {
	Image            string              `json:"Image"`
	Env              []string            `json:"Env,omitempty"`
	Cmd              []string            `json:"Cmd,omitempty"`
	Labels           map[string]string   `json:"Labels,omitempty"`
	ExposedPorts     map[string]struct{} `json:"ExposedPorts,omitempty"`
	HostConfig       HostConfig          `json:"HostConfig"`
	NetworkingConfig *NetworkingConfig   `json:"NetworkingConfig,omitempty"`
}

type HostConfig struct {
	Binds         []string                 `json:"Binds,omitempty"`
	PortBindings  map[string][]PortBinding `json:"PortBindings,omitempty"`
	RestartPolicy RestartPolicy            `json:"RestartPolicy"`
	ExtraHosts    []string                 `json:"ExtraHosts,omitempty"`
	Memory        int64                    `json:"Memory,omitempty"`
	NanoCPUs      int64                    `json:"NanoCpus,omitempty"`
}

type PortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type RestartPolicy struct {
	Name string `json:"Name"`
}

type NetworkingConfig struct {
	EndpointsConfig map[string]struct{} `json:"EndpointsConfig"`
}

type ContainerInfo struct {
	ID           string `json:"Id"`
	Name         string `json:"Name"`
	RestartCount int    `json:"RestartCount"`
	State        struct {
		Status     string `json:"Status"`
		Running    bool   `json:"Running"`
		Restarting bool   `json:"Restarting"`
		OOMKilled  bool   `json:"OOMKilled"`
		ExitCode   int    `json:"ExitCode"`
		Error      string `json:"Error"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
		Health     *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

func (i *ContainerInfo) StartedAt() time.Time {
	t, _ := time.Parse(time.RFC3339Nano, i.State.StartedAt)
	return t
}

type ContainerSummary struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Status string            `json:"Status"`
	Labels map[string]string `json:"Labels"`
}

func (c *Client) ContainerCreate(ctx context.Context, name string, cfg ContainerConfig) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	err := c.call(ctx, "POST", "/containers/create", url.Values{"name": {name}}, cfg, &out)
	return out.ID, err
}

func (c *Client) ContainerStart(ctx context.Context, id string) error {
	return c.call(ctx, "POST", "/containers/"+id+"/start", nil, nil, nil)
}

func (c *Client) ContainerStop(ctx context.Context, id string, timeout time.Duration) error {
	q := url.Values{"t": {fmt.Sprint(int(timeout.Seconds()))}}
	return c.call(ctx, "POST", "/containers/"+id+"/stop", q, nil, nil)
}

func (c *Client) ContainerRestart(ctx context.Context, id string, timeout time.Duration) error {
	q := url.Values{"t": {fmt.Sprint(int(timeout.Seconds()))}}
	return c.call(ctx, "POST", "/containers/"+id+"/restart", q, nil, nil)
}

func (c *Client) ContainerRemove(ctx context.Context, id string) error {
	return c.call(ctx, "DELETE", "/containers/"+id, url.Values{"force": {"1"}}, nil, nil)
}

func (c *Client) ContainerInspect(ctx context.Context, id string) (*ContainerInfo, error) {
	var info ContainerInfo
	if err := c.call(ctx, "GET", "/containers/"+id+"/json", nil, nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// ContainersByLabel lista contenedores (incluye detenidos) que tengan la label dada ("clave" o "clave=valor").
func (c *Client) ContainersByLabel(ctx context.Context, label string) ([]ContainerSummary, error) {
	q := filters(map[string][]string{"label": {label}})
	q.Set("all", "1")
	var out []ContainerSummary
	return out, c.call(ctx, "GET", "/containers/json", q, nil, &out)
}

// ContainerLogs devuelve las últimas `tail` líneas de stdout+stderr.
func (c *Client) ContainerLogs(ctx context.Context, id string, tail int) (string, error) {
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "tail": {fmt.Sprint(tail)}, "timestamps": {"1"}}
	resp, err := c.do(ctx, "GET", "/containers/"+id+"/logs", q, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	return demux(data), nil
}

// ContainerExec ejecuta cmd dentro del contenedor (como su usuario por defecto) y espera a que termine.
// Devuelve la salida combinada (stdout+stderr, máx. 1 MB) y el código de salida.
func (c *Client) ContainerExec(ctx context.Context, id string, cmd []string) (string, int, error) {
	var created struct {
		ID string `json:"Id"`
	}
	err := c.call(ctx, "POST", "/containers/"+id+"/exec", nil, map[string]any{
		"AttachStdout": true, "AttachStderr": true, "Cmd": cmd,
	}, &created)
	if err != nil {
		return "", -1, err
	}
	resp, err := c.do(ctx, "POST", "/exec/"+created.ID+"/start", nil, map[string]any{"Detach": false, "Tty": false})
	if err != nil {
		return "", -1, err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	io.Copy(io.Discard, resp.Body) // si la salida supera el límite, igual hay que esperar a que termine
	resp.Body.Close()
	if err != nil {
		return demux(data), -1, err
	}
	var info struct {
		Running  bool `json:"Running"`
		ExitCode int  `json:"ExitCode"`
	}
	if err := c.call(ctx, "GET", "/exec/"+created.ID+"/json", nil, nil, &info); err != nil {
		return demux(data), -1, err
	}
	return demux(data), info.ExitCode, nil
}

// ExecSession es un proceso interactivo (con TTY) dentro de un contenedor.
// Conn transporta la entrada y la salida del terminal en crudo.
type ExecSession struct {
	ID   string
	Conn io.ReadWriteCloser
}

// ExecAttach inicia cmd con TTY dentro del contenedor y devuelve la conexión al terminal.
// user vacío = el usuario por defecto de la imagen.
func (c *Client) ExecAttach(ctx context.Context, id string, cmd []string, user string, env []string) (*ExecSession, error) {
	var created struct {
		ID string `json:"Id"`
	}
	err := c.call(ctx, "POST", "/containers/"+id+"/exec", nil, map[string]any{
		"AttachStdin": true, "AttachStdout": true, "AttachStderr": true, "Tty": true,
		"Cmd": cmd, "Env": env, "User": user,
	}, &created)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(map[string]any{"Detach": false, "Tty": true})
	req, err := http.NewRequestWithContext(ctx, "POST", "http://docker/exec/"+created.ID+"/start", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(data))}
	}
	conn, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		resp.Body.Close()
		return nil, errors.New("docker: la conexión del exec no admite escritura")
	}
	return &ExecSession{ID: created.ID, Conn: conn}, nil
}

// ExecResize ajusta el tamaño del TTY de un exec interactivo.
func (c *Client) ExecResize(ctx context.Context, execID string, cols, rows int) error {
	q := url.Values{"w": {fmt.Sprint(cols)}, "h": {fmt.Sprint(rows)}}
	return c.call(ctx, "POST", "/exec/"+execID+"/resize", q, nil, nil)
}

// demux quita las cabeceras de 8 bytes que Docker agrega a cada bloque cuando el contenedor no usa TTY.
func demux(data []byte) string {
	var out bytes.Buffer
	for len(data) >= 8 && data[0] <= 2 && data[1] == 0 && data[2] == 0 && data[3] == 0 {
		size := int(binary.BigEndian.Uint32(data[4:8]))
		data = data[8:]
		if size > len(data) {
			size = len(data)
		}
		out.Write(data[:size])
		data = data[size:]
	}
	out.Write(data) // resto sin cabecera (contenedores con TTY)
	return out.String()
}

// VolumeRemove elimina un volumen (no falla si no existe).
func (c *Client) VolumeRemove(ctx context.Context, name string) error {
	err := c.call(ctx, "DELETE", "/volumes/"+name, nil, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// --- Imágenes ---

// ImagePull descarga una imagen ("traefik:v3.6") y espera a que termine.
func (c *Client) ImagePull(ctx context.Context, ref string) error {
	name, tag := ref, "latest"
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		name, tag = ref[:i], ref[i+1:]
	}
	resp, err := c.do(ctx, "POST", "/images/create", url.Values{"fromImage": {name}, "tag": {tag}}, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var msg struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) == nil && msg.Error != "" {
			return fmt.Errorf("descargando %s: %s", ref, msg.Error)
		}
	}
	return sc.Err()
}

type ImageSummary struct {
	ID       string   `json:"Id"`
	RepoTags []string `json:"RepoTags"`
	Created  int64    `json:"Created"`
}

func (c *Client) ImagesByReference(ctx context.Context, ref string) ([]ImageSummary, error) {
	var out []ImageSummary
	return out, c.call(ctx, "GET", "/images/json", filters(map[string][]string{"reference": {ref}}), nil, &out)
}

func (c *Client) ImageRemove(ctx context.Context, ref string) error {
	return c.call(ctx, "DELETE", "/images/"+ref, nil, nil, nil)
}

// --- Redes ---

func (c *Client) NetworkEnsure(ctx context.Context, name string) error {
	err := c.call(ctx, "GET", "/networks/"+name, nil, nil, nil)
	if err == nil || !IsNotFound(err) {
		return err
	}
	body := map[string]any{"Name": name, "Driver": "bridge", "CheckDuplicate": true}
	return c.call(ctx, "POST", "/networks/create", nil, body, nil)
}

// --- Métricas ---

// Stats es la parte de /containers/{id}/stats que usa el monitoreo.
type Stats struct {
	Read     time.Time `json:"read"`
	CPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemUsage uint64 `json:"system_cpu_usage"`
		OnlineCPUs  int    `json:"online_cpus"`
	} `json:"cpu_stats"`
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	} `json:"networks"`
	BlkioStats struct {
		IOServiceBytesRecursive []struct {
			Op    string `json:"op"`
			Value uint64 `json:"value"`
		} `json:"io_service_bytes_recursive"`
	} `json:"blkio_stats"`
}

// MemUsed descuenta la caché de archivos (igual que `docker stats`).
func (s *Stats) MemUsed() uint64 {
	u := s.MemoryStats.Usage
	if c, ok := s.MemoryStats.Stats["inactive_file"]; ok && c < u {
		return u - c
	}
	return u
}

// ContainerStats toma una sola muestra (sin esperar la segunda lectura de Docker).
func (c *Client) ContainerStats(ctx context.Context, id string) (*Stats, error) {
	var st Stats
	return &st, c.call(ctx, "GET", "/containers/"+id+"/stats", url.Values{"stream": {"0"}, "one-shot": {"1"}}, nil, &st)
}
