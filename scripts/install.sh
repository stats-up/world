#!/usr/bin/env bash
# Instalador de world para Ubuntu 24.04 LTS (también funciona en 22.04).
# Uso (desde la carpeta del repo clonado):  sudo ./scripts/install.sh
# Se puede volver a ejecutar sin problemas: solo completa lo que falte.
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DATA_DIR="/var/lib/world"
ENV_FILE="/etc/world/world.env"
PORT="9000"

step() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
warn() { printf '\033[1;33m[!] %s\033[0m\n' "$*"; }

if [ "$(id -u)" -ne 0 ]; then
  echo "Ejecuta el instalador con sudo:  sudo ./scripts/install.sh"
  exit 1
fi
# shellcheck disable=SC1091
. /etc/os-release
if [ "${ID:-}" != "ubuntu" ]; then
  warn "Este instalador está pensado para Ubuntu (detectado: ${PRETTY_NAME:-desconocido}). Continúo igual."
fi

step "Paquetes base"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq ca-certificates curl git >/dev/null

step "Docker"
if ! command -v docker >/dev/null 2>&1; then
  install -m 0755 -d /etc/apt/keyrings
  curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
  chmod a+r /etc/apt/keyrings/docker.asc
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu ${UBUNTU_CODENAME:-$VERSION_CODENAME} stable" \
    > /etc/apt/sources.list.d/docker.list
  apt-get update -qq
  apt-get install -y -qq docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin >/dev/null
  echo "Docker instalado: $(docker --version)"
else
  echo "Docker ya estaba instalado: $(docker --version)"
fi

# Rotación de logs: sin esto, un sitio con muchos errores puede llenar el disco.
if [ ! -f /etc/docker/daemon.json ]; then
  mkdir -p /etc/docker
  cat > /etc/docker/daemon.json <<'JSON'
{
  "log-driver": "json-file",
  "log-opts": { "max-size": "10m", "max-file": "3" }
}
JSON
  systemctl restart docker
  echo "Rotación de logs de Docker configurada."
elif ! grep -q '"max-size"' /etc/docker/daemon.json; then
  warn "/etc/docker/daemon.json ya existe y no define rotación de logs. Revisa que tenga log-opts max-size/max-file."
fi
systemctl enable --now docker >/dev/null

step "Memoria swap"
mem_mb=$(awk '/MemTotal/ {print int($2/1024)}' /proc/meminfo)
if [ "$mem_mb" -lt 2048 ] && [ -z "$(swapon --show --noheadings)" ]; then
  fallocate -l 2G /swapfile && chmod 600 /swapfile && mkswap /swapfile >/dev/null && swapon /swapfile
  grep -q '^/swapfile ' /etc/fstab || echo '/swapfile none swap sw 0 0' >> /etc/fstab
  echo "Creado swap de 2 GB (el servidor tiene ${mem_mb} MB de RAM)."
else
  swap_size="$(swapon --show --noheadings | awk '{print $3}' | head -1)"
  echo "RAM: ${mem_mb} MB. Swap: ${swap_size:-sin swap (no es necesario con 2 GB o más)}"
fi
if [ "$mem_mb" -lt 1800 ]; then
  warn "Con menos de 2 GB de RAM habrá espacio para pocos sitios. Recomendado: 2 GB o más."
fi

step "Puertos 80 y 443"
for p in 80 443; do
  if ss -ltnH "sport = :$p" | grep -q . && ! docker ps --format '{{.Names}}' | grep -qx world-traefik; then
    warn "El puerto $p ya está en uso por otro programa (¿nginx o apache?). Traefik no podrá iniciar hasta liberarlo:"
    ss -ltnpH "sport = :$p" || true
  fi
done

step "Configuración"
git config --system --get-all safe.directory 2>/dev/null | grep -qx "$REPO_DIR" \
  || git config --system --add safe.directory "$REPO_DIR"
mkdir -p "$DATA_DIR" /etc/world
chmod 700 "$DATA_DIR"
if [ ! -f "$ENV_FILE" ]; then
  branch="$(git -C "$REPO_DIR" rev-parse --abbrev-ref HEAD 2>/dev/null || echo main)"
  cat > "$ENV_FILE" <<EOF
# Configuración de world (se lee al iniciar el servicio: sudo systemctl restart world)
WORLD_LISTEN=:$PORT
WORLD_DATA_DIR=$DATA_DIR
WORLD_REPO_DIR=$REPO_DIR
# Rama que sigue el auto-update
WORLD_BRANCH=$branch
EOF
  chmod 600 "$ENV_FILE"
  echo "Creado $ENV_FILE"
else
  echo "$ENV_FILE ya existe (no se modifica)."
  PORT="$(grep -E '^WORLD_LISTEN=' "$ENV_FILE" | sed 's/.*://' || echo 9000)"
fi

step "Compilando world (en un contenedor, puede tardar 1–3 minutos la primera vez)"
bash "$REPO_DIR/scripts/build.sh"
install -m 0755 "$REPO_DIR/.build/world" /usr/local/bin/world

step "Servicios systemd"
install -m 0644 "$REPO_DIR/deploy/systemd/world.service" /etc/systemd/system/world.service
install -m 0644 "$REPO_DIR/deploy/systemd/world-update.service" /etc/systemd/system/world-update.service
install -m 0644 "$REPO_DIR/deploy/systemd/world-update.timer" /etc/systemd/system/world-update.timer
sed -i "s#@REPO_DIR@#$REPO_DIR#g" /etc/systemd/system/world-update.service
chmod +x "$REPO_DIR/scripts/"*.sh
systemctl daemon-reload
systemctl enable world >/dev/null 2>&1
systemctl restart world
systemctl enable --now world-update.timer >/dev/null 2>&1

ok=""
for _ in $(seq 1 20); do
  if curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then ok=1; break; fi
  sleep 1
done
if [ -z "$ok" ]; then
  warn "El panel no respondió. Revisa los logs con:  sudo journalctl -u world -n 50"
  exit 1
fi

ip="$(curl -fsS --max-time 5 https://checkip.amazonaws.com 2>/dev/null || hostname -I | awk '{print $1}')"
ip="$(echo "$ip" | tr -d '[:space:]')"
token=""
[ -f "$DATA_DIR/setup-token" ] && token="$(cat "$DATA_DIR/setup-token")"

printf '\n\033[1;32m✓ world instalado (%s)\033[0m\n\n' "$(/usr/local/bin/world version)"
echo "  Panel:   http://$ip:$PORT"
if [ -n "$token" ]; then
  echo "  Código de instalación (para crear el administrador):  $token"
fi
cat <<EOF

  Recuerda abrir en el firewall de Lightsail (Networking → IPv4 Firewall):
    TCP 80, TCP 443 y TCP $PORT (este último solo hasta configurar el dominio del panel).

  Comandos útiles:
    sudo journalctl -u world -f              logs del panel
    sudo $REPO_DIR/scripts/update.sh --force  forzar actualización
    sudo world reset-password <email>        recuperar acceso
EOF
