# world

Panel web liviano para publicar sitios en contenedores Docker en tu propio servidor:
dominios, subdominios y certificados SSL automáticos, deploy desde GitHub y variables
de ambiente cifradas, todo desde una interfaz web.

- **Liviano:** un solo binario en Go con SQLite. Consume pocas decenas de MB de RAM.
- **Sitios PHP/Laravel, estáticos o con Dockerfile propio.**
- **Repositorios privados de GitHub** mediante deploy keys generadas por el panel.
- **SSL automático** con Traefik y Let's Encrypt.
- **Se actualiza solo:** revisa este repositorio cada 10 minutos y, si hay cambios, se recompila y se reinicia. Si algo falla, vuelve a la versión anterior.

```
Internet ──► Traefik (:80/:443, SSL automático)
               ├── panel.tudominio.cl → panel world (servicio systemd en el host)
               ├── tusitio.cl         → contenedor del sitio (nginx + PHP-FPM)
               └── api.tusitio.cl     → contenedor de otro sitio
```

---

## Requisitos

- Un servidor con **Ubuntu 24.04 LTS**, por ejemplo una instancia de AWS Lightsail.
  - Se recomiendan **2 GB de RAM o más**. Con 1 GB funciona para pocos sitios pequeños.
- **Firewall de Lightsail** (consola de AWS → tu instancia → *Networking* → *IPv4 Firewall*). Abre estos puertos:
  - `TCP 22` (SSH, viene abierto por defecto)
  - `TCP 80` y `TCP 443` (sitios y certificados SSL)
  - `TCP 9000` (panel, **solo hasta** configurarle un dominio; después ciérralo)
- **IP estática** de Lightsail asignada a la instancia, para que tus dominios no dejen de apuntar al servidor.
- **Un dominio**, cuando quieras publicar sitios. Para probar sin dominio puedes usar `sslip.io` (ver más abajo).

## Instalación

Conéctate por SSH al servidor y ejecuta:

```bash
sudo apt update && sudo apt install -y git
```

```bash
sudo git clone https://github.com/stats-up/world.git /opt/world
```

```bash
sudo bash /opt/world/scripts/install.sh
```

El instalador:

1. Instala Docker (repositorio oficial) y configura la rotación de logs.
2. Crea memoria swap si el servidor tiene menos de 2 GB de RAM.
3. Compila el panel dentro de un contenedor (el servidor no necesita tener Go instalado).
4. Lo deja corriendo como servicio (`world`) y activa el auto-update (`world-update.timer`).
5. Al final te muestra la **URL del panel** y el **código de instalación**.

Puedes ejecutarlo de nuevo sin problemas: solo completa lo que falte.

## Primer ingreso

1. Abre `http://IP-DEL-SERVIDOR:9000`.
2. Ingresa el **código de instalación**, tu email y una contraseña. Así nadie más puede crear el administrador si llega al panel antes que tú.
   Si no anotaste el código, puedes verlo en el servidor con:
   ```bash
   sudo cat /var/lib/world/setup-token
   ```
3. En **Perfil**, activa la verificación en dos pasos (2FA). Es muy recomendable, porque quien entra al panel controla el servidor.

## Dominio del panel (HTTPS)

1. En tu proveedor de DNS, crea un registro **A**, por ejemplo `panel.tudominio.cl`, apuntando a la IP del servidor.
2. En el panel, ve a **Ajustes**, ingresa ese dominio y guarda.
3. En uno o dos minutos el panel queda disponible en `https://panel.tudominio.cl` con certificado válido.
4. Cierra el puerto `9000` en el firewall de Lightsail.

**¿Aún no tienes dominio?** Puedes usar `sslip.io`, un servicio público que resuelve cualquier nombre con la IP incluida a esa misma IP.
Por ejemplo, `panel.56-125-173-77.sslip.io` apunta a `56.125.173.77`, sin configurar nada. Sirve para probar, incluido el certificado SSL.
El panel te sugiere el nombre correcto en **Ajustes**.

## Publicar un sitio

1. **Sitios → + Nuevo sitio**.
2. **Repositorio:**
   - Privado: `git@github.com:usuario/repo.git`. El panel genera una **deploy key**. Cópiala desde la página del sitio y agrégala en GitHub (*tu repo → Settings → Deploy keys → Add deploy key*), sin marcar "Allow write access".
   - Público: `https://github.com/usuario/repo.git`.
3. **Dominios:** uno por línea. Pueden ser el dominio principal, `www` y cualquier subdominio. Cada uno necesita un registro DNS **A** apuntando al servidor.
4. **Tipo:**
   - **PHP / Laravel:** usa la imagen [`serversideup/php`](https://serversideup.net/open-source/docker-php/) (nginx + PHP-FPM).
     Elige la versión de PHP y agrega extensiones si las necesitas (`intl`, `gd`, `imagick`, etc.).
     Si el repo tiene `composer.json`, se ejecuta `composer install --no-dev`. Si tiene `package.json`, se ejecuta `npm run build` (opcional).
   - **Estático:** HTML/CSS/JS servido con nginx.
   - **Dockerfile propio:** usa el `Dockerfile` de la raíz del repo. Debes indicar el puerto interno.
5. **Variables de ambiente:** pega tu `.env` de producción. Se guardan cifradas en el servidor.
6. Presiona **Deploy** y sigue el log en vivo.

Cada deploy **construye una imagen nueva, levanta el contenedor nuevo, verifica que arranque y recién entonces retira el anterior**.
Si el contenedor nuevo falla, el sitio sigue funcionando con la versión anterior y el log muestra las últimas líneas del error.

### Notas para Laravel

- Define `APP_KEY`, `APP_URL=https://tudominio.cl` y `APP_ENV=production` en las variables de ambiente.
- La opción **"ejecutar migraciones y cachés al iniciar"** corre `migrate --force` y `optimize` en cada arranque. Actívala solo cuando la base de datos ya esté configurada.
- Por defecto se usa `LOG_CHANNEL=stderr`, así los errores aparecen en **Ver logs** del panel. Puedes cambiarlo en las variables.
- El tráfico llega a través de Traefik (proxy). Para que Laravel genere URLs `https://`, confía en el proxy. En Laravel 11 y versiones posteriores, en `bootstrap/app.php`:
  ```php
  ->withMiddleware(function (Middleware $middleware) {
      $middleware->trustProxies(at: '*');
  })
  ```

## Actualizaciones automáticas

El servidor revisa este repositorio cada 10 minutos (`world-update.timer`). Si hay commits nuevos en la rama configurada:

1. Descarga los cambios y recompila.
2. Reemplaza el binario y reinicia el panel.
3. Si el panel nuevo no responde, **vuelve a la versión anterior** y no reintenta ese commit.

Los sitios **no se reinician** cuando se actualiza el panel: siguen corriendo en sus contenedores.

La rama que sigue el auto-update se define en `/etc/world/world.env` (`WORLD_BRANCH`). Para un servidor de producción conviene usar una rama `stable`.

Para forzar una actualización:

```bash
sudo /opt/world/scripts/update.sh --force
```

Para ver el historial de actualizaciones:

```bash
sudo journalctl -u world-update -n 50
```

## Comandos útiles

| Qué | Comando |
|---|---|
| Logs del panel en vivo | `sudo journalctl -u world -f` |
| Estado del panel | `sudo systemctl status world` |
| Reiniciar el panel | `sudo systemctl restart world` |
| Recuperar contraseña | `sudo world reset-password tu@email.cl` |
| Desactivar 2FA (teléfono perdido) | `sudo world disable-2fa tu@email.cl` |
| Versión instalada | `world version` |
| Contenedores | `sudo docker ps` |

## Dónde se guardan los datos

| Ruta | Contenido |
|---|---|
| `/var/lib/world/world.db` | Base de datos del panel (SQLite): usuarios, sitios, deploys |
| `/var/lib/world/secret.key` | Llave que cifra variables de ambiente y deploy keys. **Respáldala junto con la base de datos** |
| `/var/lib/world/deployments/` | Logs de cada deploy |
| `/var/lib/world/traefik/` | Configuración de Traefik y certificados SSL |
| `/etc/world/world.env` | Configuración del servicio (puerto, rutas, rama) |

**Respaldos:** lo más simple es activar **snapshots automáticos** de la instancia en Lightsail.

## Seguridad

- El panel controla Docker, lo que equivale a acceso root al servidor. Usa una contraseña fuerte y **activa 2FA**.
- Después de configurar el dominio del panel, cierra el puerto `9000` en el firewall.
- Las variables de ambiente y las deploy keys se guardan cifradas (AES-256-GCM).
- Los intentos de login fallidos se limitan por IP.

## Desinstalar

```bash
sudo systemctl disable --now world world-update.timer
```

```bash
sudo docker rm -f world-traefik $(sudo docker ps -aq --filter label=world.site)
```

```bash
sudo rm -rf /etc/systemd/system/world*.service /etc/systemd/system/world-update.timer /usr/local/bin/world* /etc/world /var/lib/world /opt/world
```

## Hoja de ruta

- [x] **Fase 1:** instalador, auto-update, login con 2FA, sitios desde GitHub, dominios y subdominios con SSL, variables de ambiente, límites de RAM/CPU y logs
- [ ] **Fase 2:** webhook de GitHub (deploy automático al hacer push), rollback, workers de colas y scheduler de Laravel, archivos de llaves y assets compartidos
- [ ] **Dominios y correo:** DNS mediante API (registros automáticos al agregar dominios, certificados wildcard) y envío con Amazon SES (verificación de dominio, DKIM, SPF y DMARC automáticos, y credenciales `MAIL_*` para los sitios)
- [ ] **Fase 3:** crons HTTP hacia APIs con historial, y bases de datos y usuarios en MySQL/MariaDB (RDS) desde el panel
- [ ] **Fase 4:** monitoreo de recursos, registro de caídas con diagnóstico, alertas (email/Telegram) y recomendaciones de recursos

## Desarrollo

Requiere Go 1.27 o superior.

```bash
go test ./...
```

```bash
WORLD_DATA_DIR=./data WORLD_LISTEN=127.0.0.1:9000 go run ./cmd/world
```

Sin Docker local, la interfaz funciona igual. Las acciones que usan Docker muestran un error de conexión.
