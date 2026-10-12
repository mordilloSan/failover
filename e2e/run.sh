#!/usr/bin/env bash
# End-to-end test of the agent image against the local Docker.
# The "server" and the TNAS NPM are Caddy containers on bridge networks;
# btrfs is replaced by mkdir/rm and cp loses its --reflink (no btrfs here); a Caddy stands in for the
# Technitium API and Mailpit takes the alert emails.
# Covers T-04/T-05/T-06/T-11/T-12/T-19/T-20/T-21 minus btrfs. Takes ~3 min.
#
#   run.sh [test]   runs the test and removes everything
#   run.sh start    same setup in observe mode, left running for the UI
#   run.sh stop     removes what start left
set -euo pipefail

cmd=${1:-test}
here=$(cd "$(dirname "$0")/.." && pwd)
image=failover-agent-e2e
pw="e2e-password" # the account the test makes on the first access
W=$here/.e2e

cleanup() {
	docker rm -f e2e-agent e2e-server e2e-ipholder e2e-dns e2e-mail >/dev/null 2>&1 || true
	# every copy the agent started sits on e2e-tnas, whatever the service names were
	local p
	for p in $(docker ps -aq --filter network=e2e-tnas | xargs -r docker inspect -f '{{index .Config.Labels "com.docker.compose.project"}}' | sort -u); do
		(cd / && docker compose -p "$p" down -v >/dev/null 2>&1) || true
	done
	docker network rm e2e-srv e2e-tnas >/dev/null 2>&1 || true
	# the agent ran as root: its files need root to go away
	docker run --rm -v "$W:/w" --entrypoint sh "$image" -c 'rm -rf /w/*' >/dev/null 2>&1 || true
	rm -rf "$W"
}

# the UI logs in with a form and a session cookie; a restarted agent forgets the session.
# It is HTTPS with the agent's own self-signed certificate, hence -k.
ui=https://127.0.0.1:18099
login() { curl -sfk -o /dev/null -c "$W/cookies" -d username=admin -d "password=$pw" "$ui/login"; }
status() { curl -sfk -b "$W/cookies" "$ui/api/status" || { login && curl -sfk -b "$W/cookies" "$ui/api/status"; }; }

# wait_for <jq condition> <timeout s> <description>
wait_for() {
	local end=$((SECONDS + $2))
	until status | jq -e "$1" >/dev/null 2>&1; do
		if ((SECONDS > end)); then
			echo "FALHOU: $3"
			status | jq '{services, tnas_npm, events: .events[-10:]}' || true
			docker logs --tail 30 e2e-agent
			exit 1
		fi
		sleep 3
	done
	echo "ok: $3"
}

project_containers() { docker ps -aq --filter "label=com.docker.compose.project=$1" | wc -l; }

case $cmd in
test | start) cleanup ;;
stop)
	cleanup
	exit 0
	;;
*)
	echo "uso: $0 [test|start|stop]" >&2
	exit 2
	;;
esac
mode=observe
if [[ $cmd == test ]]; then
	trap cleanup EXIT
	mode=auto
fi
mkdir -p "$W"

docker build --pull -q -t "$image" -f "$here/deploy/Dockerfile" "$here" >/dev/null
docker network create --subnet 10.123.1.0/24 e2e-srv >/dev/null
# a fixed bridge name, so the agent can arping on it (lan_iface)
docker network create --subnet 10.123.2.0/24 -o com.docker.network.bridge.name=br-e2e-tnas e2e-tnas >/dev/null

# name host wait_min stability_min option
#   ml      adds a service the override must keep down (like immich's machine learning)
#   ip=X    require_free_ip X, as unifi does with its .92 (R3)
if [[ $cmd == test ]]; then
	services=("web web.test 1 1 ml" "unifi unifi.test 1 0 ip=10.123.2.92")
else
	services=("vaultwarden bitwarden.test 1 1 -" "homepage homepage.test 2 1 -" "speedtest speedtest.test 4 1 -"
		"immich immich.test 3 1 ml" "unifi unifi.test 3 0 ip=10.123.2.92")
fi
hosts=nginx.test
for s in "${services[@]}"; do
	read -r _ host _ _ _ <<<"$s"
	hosts+=", $host"
done

# The server: its NPM and every service answer at 10.123.1.10 while it runs.
printf '%s {\n\ttls internal\n\trespond "server" 200\n}\n' "$hosts" >"$W/server.Caddyfile"
docker run -d --name e2e-server --network e2e-srv --ip 10.123.1.10 \
	-v "$W/server.Caddyfile:/etc/caddy/Caddyfile:ro" caddy:alpine >/dev/null

# A stand-in for the Technitium API: every record add/delete answers ok.
cat >"$W/dns.Caddyfile" <<'EOF'
:5380 {
	respond `{"status":"ok"}` 200
}
EOF
docker run -d --name e2e-dns --network e2e-tnas --ip 10.123.2.53 \
	-v "$W/dns.Caddyfile:/etc/caddy/Caddyfile:ro" caddy:alpine >/dev/null
# The mail server: SMTP on 1025 without TLS nor login, what it got on :8025/api.
mail=10.123.2.25
docker pull -q axllent/mailpit >/dev/null
docker run -d --name e2e-mail --network e2e-tnas --ip $mail -p 127.0.0.1:18025:8025 axllent/mailpit >/dev/null

# The mirror, as the TOS backup leaves it.
m=$W/ServerBackup/homelab
mkdir -p "$m/npm" "$W/overrides" "$W/config" "$W/state"
cat >"$m/npm/docker-compose.yml" <<'EOF'
services:
  npm:
    image: caddy:alpine
    restart: unless-stopped
    volumes: [./Caddyfile:/etc/caddy/Caddyfile:ro]
    ports: ["10.123.2.1:443:443"] # like the TNAS: the NPM publishes on the host's own IP
    networks: [e2e-tnas]
networks:
  e2e-tnas: {external: true}
EOF
printf 'nginx.test {\n\ttls internal\n\trespond "tnas" 200\n}\n' >"$m/npm/Caddyfile"
# what the real NPM writes for each proxy host, read by the discovery
ph=$m/npm/data/nginx/proxy_host
mkdir -p "$ph"
# shellcheck disable=SC2016 # the $ are nginx's, written as they are
printf 'server {\n  set $forward_scheme http;\n  set $server "npm";\n  set $port 81;\n  server_name nginx.test;\n}\n' >"$ph/0.conf"
svc_config=""
for s in "${services[@]}"; do
	read -r name host wait stab opt <<<"$s"
	mkdir -p "$m/$name/html"
	printf '%s {\n\ttls internal\n\treverse_proxy %s:80\n}\n' "$host" "$name" >>"$m/npm/Caddyfile"
	n=$((${n:-0} + 1))
	# shellcheck disable=SC2016 # nginx's $, as the NPM writes them
	printf 'server {\n  set $forward_scheme http;\n  set $server "%s";\n  set $port 80;\n  server_name %s;\n}\n' "$name" "$host" >"$ph/$n.conf"
	cat >"$m/$name/docker-compose.yml" <<EOF
services:
  $name:
    image: caddy:alpine
    restart: unless-stopped
    command: caddy file-server --root /srv --listen :80
    volumes: [./html:/srv:ro]      # relative bind: only works if paths match inside and outside (T-05)
    networks: [e2e-tnas]
EOF
	override="" free_ip=""
	[[ $opt == ip=* ]] && free_ip=", require_free_ip: ${opt#ip=}"
	if [[ $opt == ml ]]; then
		# asks for a GPU, like immich's: the discovery must propose leaving it off
		printf '  ml:\n    image: e2e/machine-learning:never-pulled\n    networks: [e2e-tnas]\n    deploy: {resources: {reservations: {devices: [{capabilities: [gpu]}]}}}\n' >>"$m/$name/docker-compose.yml"
		printf 'services:\n  ml:\n    profiles: ["disabled"]\n' >"$W/overrides/$name.override.yml"
		override=", override: $name.override.yml"
	fi
	printf 'networks:\n  e2e-tnas: {external: true}\n' >>"$m/$name/docker-compose.yml"
	echo "copia do espelho" >"$m/$name/html/index.html"
	svc_config+="  - {name: $name, dir: $name, host: $host, wait_min: $wait, stability_min: $stab$override$free_ip}"$'\n'
done
# start: no services and no email, as a new install; the setup screen proposes them
email="email: {host: $mail, port: 1025, security: none, to: e2e@test.pt}"
if [[ $cmd == start ]]; then
	svc_config="  []" email=""
fi
cat >"$W/btrfs" <<'EOF'
#!/bin/sh
# stand-in for: btrfs subvolume create DST | btrfs subvolume delete PATH
case "$2" in
create) exec mkdir "$3" ;;
delete) exec rm -rf "$3" ;;
esac
exit 1
EOF
cat >"$W/cp" <<'EOF'
#!/bin/sh
# stand-in for GNU cp --reflink=always (no btrfs here): the same copy, without the reflink
for a; do
	shift
	[ "$a" = --reflink=always ] || set -- "$@" "$a"
done
exec /bin/cp "$@"
EOF
chmod +x "$W/btrfs" "$W/cp"

echo e2e-token >"$W/config/technitium.token"
cat >"$W/config/failover.yml" <<EOF
mode: $mode
server: {ip: 10.123.1.10, npm_check_host: nginx.test}
tnas_ip: 10.123.2.1 # the host's address on e2e-tnas: it answers ping, like the TNAS
router_ip: 127.0.0.1
lan_iface: br-e2e-tnas
check_interval_s: 10
start_timeout_min: 3
paths: {mirror_subvol: $W/ServerBackup, mirror_root: homelab, snapshots_dir: $W/snaps, overrides_dir: $W/overrides}
npm: {dir: npm, alert_after_min: 1}
services:
$svc_config
dns: {api_url: "http://10.123.2.53:5380", token_file: $W/config/technitium.token, zone: e2e.test, ttl: 60}
ui: {listen: "127.0.0.1:18099"}
$email
EOF

run_agent() {
	docker run -d --name e2e-agent --network host --privileged --restart unless-stopped \
		-v /var/run/docker.sock:/var/run/docker.sock -v "$W:$W" \
		-v "$W/btrfs:/usr/local/bin/btrfs:ro" -v "$W/cp:/usr/local/bin/cp:ro" -e TZ=Europe/Lisbon \
		"$image" -config "$W/config/failover.yml" -state "$W/state/state.json" >/dev/null
}
if [[ $cmd == test ]]; then
	# someone already answers on unifi's IP: the agent must refuse to start the copy (T-11)
	docker run -d --name e2e-ipholder --network e2e-tnas --ip 10.123.2.92 caddy:alpine sleep 3600 >/dev/null
fi
run_agent

end=$((SECONDS + 60))
until curl -sfk -o /dev/null "$ui/healthz"; do
	((SECONDS < end)) || {
		echo "FALHOU: a interface não responde"
		docker logs --tail 30 e2e-agent
		exit 1
	}
	sleep 2
done
if [[ $cmd == start ]]; then
	cat <<EOF

Interface: https://localhost:18099  (o browser avisa do certificado próprio)
Primeiro acesso: cria a conta, depois "Configurar em 1 minuto" propõe os serviços.
Avisos por email: "Outro servidor", $mail, porta 1025, ligação none, qualquer email;
  chegam a http://localhost:18025 (Mailpit).
Começa em observação: muda para automático na interface para ver o failover a sério.
make server-down / make server-up simula a falha do servidor · make logs · make stop
EOF
	exit 0
fi

# the first access makes the account
code=$(docker logs e2e-agent 2>&1 | sed -n 's/.*code=\([A-Z0-9]*\).*/\1/p' | tail -1)
curl -sfk -o /dev/null -d "code=$code" -d username=admin -d "password=$pw" -d "password2=$pw" "$ui/setup"
login || {
	echo "FALHOU: a conta feita no primeiro acesso não entra"
	exit 1
}
echo "ok: conta feita no primeiro acesso"

wait_for '.services[0].state == "NORMAL" and .services[0].server_ok and .server_npm_ok' 40 "servidor saudável, nada a fazer"
# the Caddys' certificates are their own CA's: they do not verify, and that is no failure
wait_for '.events | any(.msg | test("certificado de nginx.test inválido"))' 30 "certificado que não verifica: aviso, conta como a responder"
if [[ $(curl -s -o /dev/null -w '%{http_code} %{redirect_url}' http://127.0.0.1:18099/x) != "301 https://127.0.0.1:18099/x" ]]; then
	echo "FALHOU: http:// na porta da interface não redireciona para https://"
	exit 1
fi
if ! docker exec e2e-agent failover-agent healthcheck -config "$W/config/failover.yml" -state "$W/state/state.json"; then
	echo "FALHOU: o healthcheck não confia no certificado da interface"
	exit 1
fi
echo "ok: interface em HTTPS, http:// redireciona e o healthcheck confia no certificado"
disc=$(curl -sfk -b "$W/cookies" "$ui/api/discover")
if ! jq -e '(.services | map({(.dir): .}) | add) as $s | .network.npm_check_host == "nginx.test"
	and $s.web.host == "web.test" and $s.unifi.host == "unifi.test" and ($s.web.override_yaml | test("ml:"))' <<<"$disc" >/dev/null; then
	echo "FALHOU: a descoberta não deu os endereços, o NPM e o override: $disc"
	exit 1
fi
echo "ok: descoberta: endereço de cada serviço pelo NPM do espelho e o ml com GPU desligado"
# the ml image is never on disk: listing it would mean the override was ignored
wait_for '(.images.web.images | length == 1 and .[0].ref == "caddy:alpine" and .[0].present and .[0].size > 0) and .images.npm.images[0].present' 60 \
	"imagens de cada stack verificadas no TNAS, com o override aplicado (O4)"

docker stop e2e-server >/dev/null
wait_for '.services[0].state == "ACTIVE" and .services[0].dns and .tnas_npm.ok' 180 "servidor parado: failover para o TNAS, com o DNS mudado"
body=$(curl -sk --resolve web.test:443:10.123.2.1 https://web.test/)
[[ $body == "copia do espelho" ]] || {
	echo "FALHOU: a cópia não serve o espelho (T-05): $body"
	exit 1
}
echo "ok: a cópia serve os dados do snapshot pelo NPM do TNAS"
end=$((SECONDS + 60))
until curl -sf "http://$mail:8025/api/v1/messages" | jq -e '.messages | any(.Subject | test("em failover no TNAS")) and any(.To[].Address == "e2e@test.pt")' >/dev/null; do
	((SECONDS < end)) || {
		echo "FALHOU: sem email do failover: $(curl -s "http://$mail:8025/api/v1/messages" | jq -c '[.messages[] | .Subject]')"
		exit 1
	}
	sleep 3
done
echo "ok: email do failover recebido"
[[ $(project_containers failover-web) -eq 1 ]] || { # web only: ml disabled by the override
	echo "FALHOU: o override não desligou o ml"
	exit 1
}
echo "ok: override aplicado (ml desligado)"
wait_for '.services[1].state == "ERROR" and (.services[1].msg | test("ocupado"))' 30 "unifi com o IP ocupado: recusa arrancar (R3)"
[[ $(project_containers failover-unifi) -eq 0 ]] || {
	echo "FALHOU: a cópia do unifi arrancou com o IP ocupado"
	exit 1
}

snap=$(status | jq -r '.services[0].snapshot')
docker restart e2e-agent >/dev/null
wait_for ".services[0].state == \"ACTIVE\" and .services[0].snapshot == \"$snap\"" 30 "reinício do agente: estado retomado (T-21)"

docker start e2e-server >/dev/null
wait_for '.services[0].state == "NORMAL" and .services[1].state == "NORMAL" and (.tnas_npm.snapshot // "") == ""' 180 "servidor de volta: regresso"
[[ $(project_containers failover-web) -eq 0 && $(project_containers failover-npm) -eq 0 ]] || {
	echo "FALHOU: sobraram containers de cópia (R1)"
	exit 1
}
[[ -z $(ls -A "$W/snaps") ]] || {
	echo "FALHOU: sobraram snapshots"
	exit 1
}
echo "ok: containers e snapshots removidos"
echo "E2E OK"
