#!/usr/bin/env bash
# Auto-update: si hay commits nuevos en la rama configurada, recompila, reemplaza el binario
# y reinicia el panel. Si el panel nuevo no responde, vuelve a la versión anterior.
# Lo ejecuta el timer world-update.timer; también se puede correr a mano: sudo /opt/world/scripts/update.sh
#
# Todo va dentro de main(): bash lee los scripts a medida que avanza, y `git reset` puede
# reemplazar este mismo archivo mientras corre.
set -euo pipefail

main() {
  # shellcheck disable=SC1091
  [ -f /etc/world/world.env ] && . /etc/world/world.env
  local repo="${WORLD_REPO_DIR:-/opt/world}"
  local branch="${WORLD_BRANCH:-main}"
  local port="${WORLD_LISTEN:-:9000}"
  port="${port##*:}"
  local force="${1:-}"

  exec 9>/run/world-update.lock
  if ! flock -n 9; then
    echo "Ya hay una actualización en curso."
    return 0
  fi

  cd "$repo"
  # Sin esto, si el repo no es accesible git se queda esperando un usuario/contraseña.
  export GIT_TERMINAL_PROMPT=0
  git fetch --quiet origin "$branch"
  local current remote
  current="$(git rev-parse HEAD)"
  remote="$(git rev-parse "origin/$branch")"
  if [ "$current" = "$remote" ] && [ "$force" != "--force" ]; then
    return 0
  fi
  # Un commit que ya falló no se reintenta cada 10 minutos (salvo con --force).
  local failed_file="${WORLD_DATA_DIR:-/var/lib/world}/update-failed"
  if [ "$force" != "--force" ] && [ -f "$failed_file" ] && [ "$(cat "$failed_file")" = "$remote" ]; then
    return 0
  fi

  echo "Actualizando world: ${current:0:7} → ${remote:0:7}"
  git reset --hard --quiet "origin/$branch"

  if ! bash scripts/build.sh; then
    echo "La compilación falló: se mantiene la versión actual." >&2
    echo "$remote" > "$failed_file"
    git reset --hard --quiet "$current"
    return 1
  fi

  install -m 0644 deploy/systemd/world.service /etc/systemd/system/world.service
  install -m 0644 deploy/systemd/world-update.service /etc/systemd/system/world-update.service
  install -m 0644 deploy/systemd/world-update.timer /etc/systemd/system/world-update.timer
  sed -i "s#@REPO_DIR@#$repo#g" /etc/systemd/system/world-update.service
  systemctl daemon-reload

  [ -f /usr/local/bin/world ] && cp -f /usr/local/bin/world /usr/local/bin/world.prev
  install -m 0755 .build/world /usr/local/bin/world
  systemctl restart world

  local ok=""
  for _ in $(seq 1 15); do
    if curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1; then ok=1; break; fi
    sleep 2
  done
  if [ -z "$ok" ]; then
    echo "El panel nuevo no responde: volviendo a la versión anterior." >&2
    if [ -f /usr/local/bin/world.prev ]; then
      install -m 0755 /usr/local/bin/world.prev /usr/local/bin/world
      systemctl restart world
    fi
    echo "$remote" > "$failed_file"
    git reset --hard --quiet "$current"
    return 1
  fi
  rm -f "$failed_file"
  echo "world actualizado a $(/usr/local/bin/world version)"
}

main "$@"
exit $?
