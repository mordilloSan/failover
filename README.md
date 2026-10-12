# failover-agent

Corre no TNAS e vigia os serviços do servidor (`.66`). Quando um falha, arranca uma cópia no TNAS a partir de um snapshot do espelho (uma cópia Btrfs com reflink: segundos, quase sem espaço) e aponta o nome do serviço para o TNAS no Technitium. Quando o servidor volta, repõe o DNS e apaga a cópia. **O que se escreveu na cópia perde-se.**

## Como funciona

A cada verificação (60 s por defeito), o agente pede `https://<serviço>` ao NPM do servidor.

1. Um serviço que falha durante a **espera antes do failover** (2 min por defeito) passa para o TNAS: snapshot do espelho, as imagens em falta descarregadas, `docker compose up` da cópia, verificação da cópia pelo NPM do TNAS (a cada 5 s, até responder) e registo DNS. Cada passo fica com a hora e aparece na página enquanto acontece.
2. Quando volta a responder no servidor durante a **estabilidade antes do regresso** (2 min), o agente repõe o DNS e remove a cópia e o snapshot.
3. Com o router em baixo, o agente não decide nada. Com o NPM do servidor em baixo mas o servidor vivo, os serviços ficam no servidor e o agente avisa.
4. Com o NPM do servidor sem resposta há 1 min, com o servidor ligado ou não, o NPM do TNAS liga-se, mesmo sem nenhum serviço em failover. A [app Failover do Technitium](#o--da-zona-no-technitium) manda então o resto dos nomes (`*`) para o TNAS: o que não vive no servidor, como a interface do TNAS ou do router, continua acessível, e o que só existe no servidor dá 502 em vez de ficar à espera. O NPM do TNAS desliga-se quando o NPM do servidor volta e nenhuma cópia precisa dele.
5. Um failover que dá erro é tentado outra vez a cada `start_timeout_min` (10 min) enquanto o serviço não responder no servidor, em automático e fora de manutenção.

Em **observação** só regista nos eventos o que faria. Em **automático** faz tudo sozinho.

## Instalar

No TNAS, em `/Volume1/Docker/failover/`, com o [docker-compose.yml](deploy/docker-compose.yml):

```bash
docker compose pull && docker compose up -d
```

1. Abre `https://192.168.1.249:8099` nos **30 minutos** a seguir ao arranque e cria a conta de administrador com o código que o agente escreve no arranque (`docker logs failover-agent`). O browser avisa do certificado: é o do próprio agente, aceita-o uma vez.
2. **Configurar em 1 minuto:** confirma a rede e os caminhos que o agente encontrou, cola o token da API do Technitium (Administração → Sessões → Criar token), escolhe o email dos avisos e marca os serviços a proteger. **Começar** guarda tudo, faz um snapshot de teste e envia um email de teste. Cada bloco diz se correu bem.
3. Começa em observação. Quando os eventos mostrarem o que esperas, passa a automático em ⚙ Definições → Geral.

### O `*` da zona no Technitium

O agente só muda os nomes dos serviços que passa para o TNAS. O resto da zona segue o registo `*`, e é a app **Failover** do Technitium que o manda para o TNAS quando o NPM do servidor deixa de responder:

1. **O DNS do próprio TNAS:** no TOS, nas definições de rede, põe `192.168.1.249` primeiro e `192.168.1.66` depois. Só com o `.66`, o TNAS fica sem nomes quando o servidor cai: o NPM do TNAS não instala os plugins do certbot e os emails não saem.
2. Em ⚙ Definições → DNS → **Configurar no Technitium**, o agente instala a app Failover em cada Technitium do cluster que responda e grava o `*` como registo da app (TTL do registo, `Failover.Address`). Um `*` A que lá esteja sai a seguir, porque com ele a app é ignorada. Os dados do registo:

   ```json
   {
     "primary": ["192.168.1.66"],
     "secondary": ["192.168.1.249"],
     "serverDown": ["192.168.1.249"],
     "healthCheck": "tcp443",
     "healthCheckUrl": null,
     "allowTxtStatus": true
   }
   ```

   O `tcp443` testa o NPM do servidor e não só a máquina: um servidor ligado sem os containers também manda os nomes para o TNAS. O `serverDown` mantém o TNAS mesmo que o teste ao `.249` falhe. A app testa a cada 60 s, 3 vezes, por isso troca uns 3 min depois da falha. Um Technitium que não responda (o do servidor, com ele em baixo) fica para quando voltar: carrega outra vez em Configurar.
3. "Pronto para failover" verifica tudo isto: a app em cada Technitium que responda e o `*` com estes dados, sem nenhum A.
4. Confirma nos dois servidores (o do `.66` tem a zona como secundária):

   ```bash
   nslookup qualquer.engmariz.com 192.168.1.249   # 192.168.1.66
   nslookup qualquer.engmariz.com 192.168.1.66    # 192.168.1.66
   nslookup -type=TXT qualquer.engmariz.com 192.168.1.249   # healthStatus=Healthy
   ```

   Se o `.66` não responder com um endereço, a zona secundária não serve a app: volta a pôr o `*` A `192.168.1.66` e diz-me.

**Atualizar:** `docker compose pull && docker compose up -d`. A versão instalada: `docker compose run --rm failover-agent version`.

**Password perdida:** `docker exec failover-agent rm /config/user.yml && docker restart failover-agent`, e cria a conta outra vez nos 30 minutos seguintes, com o código novo que o agente escreve no arranque (`docker logs failover-agent`).

## Usar

- **Página principal:** a topologia (clientes, servidor, TNAS, internet), os serviços e os eventos.
- **Forçar failover ou regresso:** arrasta um serviço do servidor para o TNAS, ou ao contrário, ou usa os botões no painel do serviço. Um failover forçado **fica no TNAS** até um regresso forçado; um automático volta sozinho.
- **Manutenção** (global ou por serviço): bloqueia os failovers durante o tempo escolhido; os regressos continuam.
- **Serviços:** adicionam-se em Definições → Serviços, a partir das pastas do espelho ou à mão. A pasta, o endereço, o override e o IP só mudam com o serviço no servidor.
- **Versão:** ao lado do nome, no topo; leva à release no GitHub.
- **Ícones:** o nome de um ícone do [dashboardicons.com](https://dashboardicons.com), ou o link da sua página. Vazio: o do nome ou da pasta.
- **Pronto para failover:** depois de cada verificação das imagens (no arranque, na descarga noturna e quando a stack de um serviço muda), o agente testa o token do Technitium e a app Failover com o `*` da zona, faz um snapshot de teste do espelho e vê as imagens e o que o TNAS precisa para cada compose (redes externas, portas livres). A caixa do TNAS diz "Pronto para failover" ou quantos problemas há; um problema novo vai por email.
- **Espelho:** de hora a hora o agente vê se o backup do TOS continua a escrever no espelho (`btrfs subvolume find-new`: ficheiros com dados novos) e se há pastas novas ou composes mudados. Espelho parado há mais de `mirror_stale_days` (2 por defeito), pasta nova ou compose mudado: email; a pasta nova aparece na página até a protegeres ou dispensares.
- **Avisos por email:** o servidor em baixo (com a hora), failover e regresso (o assunto diz quantos serviços, quando e quanto tempo ficaram no TNAS), erro, NPM do servidor em falha, router, certificado inválido, descarga noturna falhada, em observação o que o agente faria, e o agente a reiniciar. Os de uma mesma verificação seguem num só email. Um envio falhado aparece logo nos eventos e tenta-se de novo durante um dia.
- **Telemóvel:** no browser, "Adicionar ao ecrã inicial" instala a página. Para arrastar um serviço, segura-o meio segundo.
- **Eventos:** 30 dias, com filtro, procura e exportação para CSV.
- **Homepage (ou outro painel):** `GET /api/` dá o resumo em JSON: modo, pronto para failover, servidor, TNAS, router, internet, cluster DNS, serviços (quantos no TNAS e a lista), imagens, espelho, certificado e o último evento. Pede o token gerado em Definições → Conta (mostrado uma vez; gerar outro invalida o anterior). Com o widget `customapi` do Homepage:

  ```yaml
  - Failover:
      href: https://192.168.1.249:8099
      widget:
        type: customapi
        url: https://192.168.1.249:8099/api/
        headers:
          Authorization: Bearer {{HOMEPAGE_VAR_FAILOVER_TOKEN}}
        mappings:
          - field: services.failover
            label: Em failover
          - field: ready.ok
            label: Pronto
            remap: [{value: true, to: sim}, {value: false, to: não}]
          - field: internet.ok
            label: Internet
            remap: [{value: true, to: OK}, {value: false, to: sem acesso}]
          - field: dns.up
            label: DNS
            suffix: /2
  ```

  O certificado do agente é autoassinado: monta-o no contentor do Homepage e aponta `NODE_EXTRA_CA_CERTS=/…/state/certificates/0-self-signed.cert` para ele (o certificado inclui o IP do TNAS).

## Quando algo corre mal

| O que se vê | O que fazer |
|---|---|
| A página diz que já não dá para criar a conta | `docker restart failover-agent` e cria-a nos 30 minutos seguintes, com o código que ele escreve em `docker logs failover-agent` |
| Gmail: `535 Username and Password not accepted` | Usa uma [password de aplicação](https://myaccount.google.com/apppasswords) (precisa da verificação em 2 passos), não a password da conta |
| Um serviço "sem endereço" na descoberta | O proxy host do NPM tem de apontar para o nome ou hostname do contentor, para o IP fixo de uma macvlan, ou para o IP do servidor numa porta que o compose publica |
| "O docker compose config não deu nenhum serviço" | O compose no espelho só tem `profiles`, só tem `include`, ou falta-lhe o `.env` |
| O snapshot de teste falha | A pasta dos snapshots tem de estar no mesmo volume Btrfs do espelho (a cópia é com reflink) |
| Na cópia, um serviço não abre as próprias pastas (`Permission denied`, como a postgres ou o rabbitmq do unifi) | Vê se as pastas da cópia têm um `+` em `ls -l`: são as permissões das partilhas do TOS, que só deixam entrar o root e o dono de cada pasta. A cópia não as devia ter: é um subvolume novo, que não as herda, com os ficheiros copiados sem elas |
| "Certificado de X inválido" | O serviço conta como a responder, sem failover (o TNAS serve o mesmo certificado). Renova o certificado no NPM |
| Um serviço em ERROR | A cópia já foi removida. O agente tenta outra vez a cada 10 min enquanto o serviço não responder no servidor; entretanto podes corrigir o serviço (conta como estando em casa). A mensagem diz porquê (por exemplo, o IP da macvlan ocupado) |
| "N problemas para um failover" | O painel do TNAS lista-os: token do Technitium, snapshot de teste, imagens em falta, uma rede externa que não existe no TNAS (`docker network create <rede>`), uma porta já ocupada |
| Com o servidor em baixo, um nome que não vive no servidor (TNAS, router) não abre | Vê o `*` da zona: tem de ser o registo APP da [app Failover](#o--da-zona-no-technitium). No painel do TNAS, o NPM do TNAS tem de estar "A servir" |
| "O espelho não muda desde…" | O backup do TOS parou: vê a tarefa de backup no TOS. Um failover arrancaria com os dados desse dia |
| "Os dados de /x não estão no espelho" | O compose monta uma pasta fora do espelho, ou um volume com nome: no TNAS a cópia arranca sem esses dados. Muda o compose para uma pasta dentro da pasta do serviço |
| "O agente reiniciou depois de parar sem ser pedido" | O watchdog viu a verificação parada ou a página sem responder, ou houve um crash. O email traz o último evento; `docker logs failover-agent` tem o resto |

## Ficheiros

| Onde | O quê |
|---|---|
| `config/failover.yml` | a configuração; a interface reescreve-o ao gravar (sem os comentários) |
| `config/user.yml` | a conta, com a password em bcrypt |
| `config/technitium.token` | o token que o agente criou no Technitium |
| `state/state.json` | onde está cada serviço; sobrevive a reinícios |
| `state/events.jsonl` | os eventos (30 dias, até 5000) |
| `state/certificates/`, `state/icons/` | o certificado da interface e os ícones descarregados |
| `overrides/<serviço>.override.yml` | o que muda num compose ao correr no TNAS (macvlan, GPU desligada) |

## Desenvolvimento

```bash
make test         # go test -race (~15 s)
make lint         # gofmt, go vet, golangci-lint, shellcheck, shfmt
make e2e          # ~3 min: a imagem com Docker real, btrfs trocado por cp/rm
make start        # demo em https://localhost:18099, como uma instalação nova; emails em http://localhost:18025
make server-down  # simula a falha do servidor (server-up para voltar)
make logs / stop
```

O `e2e` e o `start` usam os mesmos containers: não corras os dois ao mesmo tempo. Só se testa no TNAS: o Btrfs a sério, o `arping` sobre o `ovs_eth0` e as macvlans.

O merge de um PR de `dev/vX.Y.Z` para o `main` cria a tag `vX.Y.Z` e corre o [release.yml](.github/workflows/release.yml): testes, imagem `ghcr.io/mordillosan/failover-agent` e binário na release, com o texto do PR como notas. Uma tag `v*` enviada à mão faz o mesmo.
