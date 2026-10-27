# kura-agent

[![CI](https://github.com/E-C-G/kura-agent/actions/workflows/ci.yml/badge.svg)](https://github.com/E-C-G/kura-agent/actions/workflows/ci.yml)

Agent de sauvegarde Windows vers Wasabi : un wrapper de `aws s3 sync` lancé par une tâche
planifiée, avec garde-fou contre les suppressions massives, fermeture d'Outlook,
journaux et état lisible par l'icône de notification.

Console d'administration associée : [E-C-G/kura-ctl](https://github.com/E-C-G/kura-ctl).

## État d'avancement

| Étape | Contenu | État |
| --- | --- | --- |
| 1 | `pkg/config` : format YAML, `${PROFILE:compte}`, validation | **fait** |
| 2 | `kura-agent run` sans interface : garde-fou, nouvelles tentatives, codes de sortie, `status.json`, journaux | **fait** |
| 3 | Bilan vers healthchecks.io ou un webhook (Make, n8n…), coffre de secrets `credentials` | **fait** |
| 4 | `install` / `uninstall` : tâche `\Kura\Sync` sous SYSTEM, ACL, clés Wasabi dans le coffre | **fait** |
| 5 | Icône par session : état, préavis et fermeture douce d'Outlook, relance, annulation, suspension | **fait** |
| 5 bis | `import` : clé d'un paquet `.kura` préparé par la console kura-ctl | **fait** |
| 5 ter | `install --package` : poste installé en une fois depuis un paquet de la console, CLI AWS comprise | **fait** |
| 6 | Restauration (V2) | à faire |

Commandes disponibles : `run`, `validate`, `status`, `credentials`, `import`, `install`, `uninstall`,
`suspend`, `tray`, `version`.

## Systèmes pris en charge

Windows (10, 11, Server) seulement pour l'instant ; Linux (Ubuntu 24.04 d'abord, Fedora ensuite)
puis macOS sont prévus. Le cœur de l'agent (configuration, garde-fou, synchronisation, bilan,
paquet `.kura`, état) est commun et testé partout ; les fonctions qui dépendent du système sont
séparées, un fichier par système, et recensées dans `internal/platform` :

| Fonction | Paquets | Windows | Linux, macOS |
| --- | --- | --- | --- |
| planification | `schedtask`, `systemd` | oui | Linux : oui (timer systemd) ; macOS : non pris en charge |
| coffre des secrets | `creds` | oui | Linux : oui (systemd-creds) ; macOS : non pris en charge |
| installation | `setup`, `install` | oui | Linux : oui ; macOS : non pris en charge |
| CLI AWS automatique | `awsinstall` | oui | Linux : oui (zip signé PGP) ; macOS : non pris en charge |
| icône et canal vers l'icône | `tray`, `ipc` | oui | non pris en charge |
| fermeture d'applications | `procs` (pré-tâches) | oui | non pris en charge |

Hors Windows, une commande qui a besoin d'une fonction absente s'arrête tout de suite et dit ce
qui manque. Sous Linux, toutes les commandes fonctionnent sauf `tray` (avec `sudo` pour le coffre
et l'installation) ; une configuration avec `pre_tasks` ou `post_tasks` y est refusée (fermeture
d'applications et icône à venir). Sous macOS :

```text
$ sudo kura-agent install
kura-agent install : non pris en charge sous macOS dans cette version (manque : installation,
planification, coffre des secrets, installation de la CLI AWS).
```

### Installer sous Linux (Ubuntu 24.04)

```bash
sudo kura-agent install --config config.yaml        # ou --package <fichier>.kura
```

`install` (rejouable, comme sous Windows) :

1. crée le compte de service `kura-agent` s'il manque (`systemd-sysusers`) ;
2. prépare `/var/lib/kura` et `/etc/kura`, et le coffre `/etc/credstore.encrypted` (root) ;
3. installe la CLI AWS si elle manque (zip signé, voir plus bas), dans un dossier de travail de
   root (`/var/cache/kura-install`) ;
4. range les secrets du paquet, installe la configuration dans `/etc/kura/config.yaml` (ancienne
   version gardée en `.bak`) ;
5. garde l'exécutable s'il est déjà dans `/usr/bin` ou `/usr/local/bin`, sinon le copie dans
   `/usr/local/bin/kura-agent` ;
6. demande les secrets manquants, lance `validate` et s'arrête là en cas d'échec ;
7. écrit `kura-sync.service` et `kura-sync.timer`, puis active le timer.

Ensuite : `sudo systemctl start kura-sync.service` (une vraie exécution, comme planifiée),
`kura-agent status`, `systemctl list-timers kura-sync.timer`, `journalctl -u kura-sync.service`.
`tray.allow_run_now` est ignoré sous Linux pour l'instant.

`sudo kura-agent uninstall` retire le timer, le service et l'exécutable copié dans
`/usr/local/bin` ; `--purge` efface aussi les secrets `kura.*` du coffre, `/var/lib/kura`,
`/var/log/kura`, `/etc/kura` et `/var/cache/kura-install`. Le compte `kura-agent` est gardé.

Sous Linux, la planification est un timer systemd, `kura-sync.timer`, qui lance le service
`kura-sync.service` (`kura-agent run --quiet --scheduled`). Les deux unités sont générées depuis
`schedule` avec la même règle que la tâche Windows, vérifiée en CI contre `systemd-analyze` :
`daily` et `weekly` à l'heure dite les jours choisis ; `hourly` toutes les heures à la minute dite,
par séries de 24 h qui démarrent à l'heure dite les jours choisis. `run_if_missed` devient
`Persistent=true`, `max_duration` + 15 min la limite de systemd. Le service tourne sous le compte
`kura-agent`, avec la seule capacité `CAP_DAC_READ_SEARCH` (lecture de tout le disque, transmise à
aws), en écriture dans `/var/lib/kura` et `/var/log/kura` seulement. `install` les mettra en place
quand l'installation Linux sera écrite.

Sous Linux, chaque secret est chiffré par `systemd-creds` (clé de la machine, et puce TPM si elle
existe) dans `/etc/credstore.encrypted/kura.<nom>`, réservé à root ; le nom est inscrit dans le
chiffré, un fichier renommé est refusé. Le service ne lit jamais ces fichiers : systemd les lui
déchiffre au démarrage (`ImportCredential=kura.*`) dans `$CREDENTIALS_DIRECTORY`, lisible par lui
seul. Les commandes lancées avec `sudo` (`credentials`, `import`, `validate`) les déchiffrent
elles-mêmes. Le compte de service `kura-agent` (système, sans mot de passe ni shell, dossier
`/var/lib/kura`) est créé par `systemd-sysusers` s'il manque.

Sous Linux, la CLI AWS absente s'installe depuis le zip officiel de l'architecture
(`awscli-exe-linux-x86_64.zip` ou `-aarch64.zip`) et sa signature `.sig`, fournis à côté du paquet
ou téléchargés chez AWS. La signature est vérifiée par `gpgv` avec la clé « AWS CLI Team »
intégrée à l'agent, dont l'empreinte est contrôlée (`FB5D B77F D5C1 18B8 0511 ADA8 A631 0ACC 4672
475C`, valable jusqu'au 1er juillet 2027 : une clé prolongée ou nouvelle demande une mise à jour de
kura-agent) ; puis `aws/install` installe dans `/usr/local/aws-cli`, lien `/usr/local/bin/aws`.

`KURA_DEV=1` autorise, pour le développement uniquement, des remplaçants non sûrs (secrets non
chiffrés, processus lus dans `/proc`). Emplacements prévus : `/var/lib/kura`, `/etc/kura` et
`/var/log/kura` sous Linux ; `/Library/Application Support/Kura` et `/Library/Logs/Kura` sous
macOS (`internal/paths`). Les exclusions (`--exclude`) suivent les règles du système : sans casse
ni sens des barres sous Windows, chemins Unix tels quels ailleurs.

## Compiler

Go 1.26 ou plus récent. Première fois, avec accès à Internet :

```sh
go mod tidy          # télécharge les dépendances et crée go.sum
```

Puis, depuis n'importe quel système :

```sh
make test            # ou : go test ./...
make windows         # produit dist/kura-agent.exe (Windows x64)
```

Sans `make` :

```sh
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X main.version=0.2.0 -X main.commit=$(git rev-parse --short HEAD)" \
  -o dist/kura-agent.exe ./cmd/kura-agent
```

## Intégration continue et publication

**CI** (`.github/workflows/ci.yml`), à chaque push sur `main` et à chaque pull request :

- Linux : `go.mod`/`go.sum` à jour, `gofmt`, `go vet` pour Linux et Windows, tests avec détection de
  concurrence, puis `kura-agent.exe` conservé 14 jours comme artefact du build ;
- Windows : tests sur un vrai Windows (verrou, chemins, remplacement de fichiers) et lancement du binaire.

**Publication** (`.github/workflows/release.yml`), déclenchée par un tag :

```sh
git tag v0.2.0
git push origin v0.2.0
```

Le workflow relance les tests, compile `kura-agent.exe` avec la version du tag, le signe si un
certificat est configuré, puis publie une release GitHub avec :

- `kura-agent_0.2.0_windows_amd64.zip` : l'exécutable, le README et `config.example.yaml` ;
- `kura-agent_0.2.0_windows_amd64.exe` : l'exécutable seul ;
- `SHA256SUMS` : les sommes de contrôle.

Un tag avec suffixe (`v0.3.0-rc1`) publie une pré-version.

**Signature Authenticode** : pas de certificat pour l'instant, l'exécutable est publié non signé.
Chaque release l'indique alors en tête de ses notes, avec la marche à suivre face à SmartScreen
(*Informations complémentaires* → *Exécuter quand même*, ou `Unblock-File`) et la vérification de
l'empreinte avec `Get-FileHash` contre `SHA256SUMS`. Certains antivirus peuvent aussi signaler un
exécutable Go non signé : prévoir une exclusion pour `C:\Program Files\Kura\` si besoin.

Pour signer plus tard, ajouter dans *Settings → Secrets and variables → Actions* :

| Secret | Contenu |
| --- | --- |
| `WINDOWS_SIGNING_CERT` | certificat de signature de code `.pfx`, encodé en base64 |
| `WINDOWS_SIGNING_PASSWORD` | mot de passe du `.pfx` |

Pour encoder le certificat sous Windows :
`[Convert]::ToBase64String([IO.File]::ReadAllBytes("cert.pfx")) | Set-Clipboard`.

**Dependabot** (`.github/dependabot.yml`) propose chaque semaine les mises à jour des modules Go et
des actions GitHub.

## Installer sur un serveur (étape 4)

Prérequis : une invite de commandes **« Exécuter en tant qu'administrateur »**, le zip de la release
décompressé. La CLI AWS v2 est installée par `install` si elle manque (voir plus bas).

Le plus simple : préparer le poste dans la console [kura-ctl](https://github.com/E-C-G/kura-ctl), qui
remet un kit (exécutables, MSI de la CLI AWS, paquet `.kura` avec configuration et secrets) ; voir
[Installer depuis un paquet de la console](#installer-depuis-un-paquet-de-la-console). À la main :

1. Préparer la configuration à partir de `configs/example.yaml` : remplacer les valeurs fictives
   (`acme-srv01`, `alice`, région, chemins) et garder les secrets hors du fichier :
   ```yaml
   profile:
     aws_cli: "C:/Program Files/Amazon/AWSCLIV2/aws.exe"
     aws_credentials: credential:wasabi     # clés d'accès Wasabi, saisies pendant l'installation
   notify:
     report:
       type: healthchecks
       url: credential:hc-url               # URL de ping, saisie pendant l'installation
   ```
   `notice_seconds` est le préavis affiché par l'icône avant de fermer Outlook (120 s dans
   l'exemple) ; `0` reproduit un script `taskkill /F`.
2. Installer, depuis le dossier du zip (qui contient `kura-agent.exe` et `kura-agentw.exe`) :
   ```bat
   kura-agent.exe install --config C:\chemin\vers\config.yaml
   ```
   `install` est rejouable : relancé avec un nouvel exécutable ou une configuration modifiée, il met à
   jour l'installation (l'ancienne configuration est gardée en `config.yaml.<date>.bak`). Il :
   - protège `C:\ProgramData\Kura` (Utilisateurs en lecture seule, coffre réservé aux administrateurs) ;
   - copie la configuration dans `C:\ProgramData\Kura\config.yaml` et l'exécutable dans
     `C:\Program Files\Kura\` ;
   - demande les secrets référencés par `credential:` qui manquent au coffre (saisie masquée) ;
   - lance `validate` (configuration, aws.exe, accès au bucket, bilan) et **s'arrête là** en cas
     d'échec, sans toucher à la tâche ;
   - crée ou remplace la tâche **`\Kura\Sync`** : compte **SYSTEM** (aucun mot de passe ; il lit
     le profil des utilisateurs et arrête Outlook dans leur session), autorisations maximales,
     déclencheur tiré de `schedule`, rattrapage si `run_if_missed`, pas de seconde instance, arrêt
     par Windows à `max_duration` + 15 min (l'agent s'arrête lui-même à `max_duration`), lancement
     autorisé aux comptes de `tray.allow_run_now` ; action `run --quiet --scheduled` ;
   - supprime le compte `svc-kura` des versions 0.3.0 à 0.4.1 s'il existe ;
   - copie `kura-agentw.exe` (le même programme, sans console) et le lance à chaque ouverture de
     session (`HKLM\...\Run`) : l'icône ne reste ouverte que pour les comptes de `tray.users`. Dans
     une session déjà ouverte, la lancer à la main : `"C:\Program Files\Kura\kura-agentw.exe" tray`.
3. Vérifier :
   ```bat
   "C:\Program Files\Kura\kura-agent.exe" run --dry-run
   schtasks /Run /TN \Kura\Sync
   "C:\Program Files\Kura\kura-agent.exe" status
   ```
   La simulation ne ferme pas Outlook et ne modifie rien dans le bucket. `schtasks /Run` lance une
   vraie exécution sous SYSTEM, comme la nuit.

### CLI AWS absente

Si `aws` n'est ni dans le PATH ni dans `C:\Program Files\Amazon\AWSCLIV2` (et que
`profile.aws_cli` est vide ou désigne ce dossier), `install` installe la CLI AWS v2 juste après
avoir protégé `C:\ProgramData\Kura` :

1. MSI fourni : `--aws-cli FICHIER` (ancien nom : `--aws-cli-msi`), sinon `AWSCLIV2.msi` posé à
   côté du paquet `.kura` ou de `kura-agent.exe` ;
2. sinon, téléchargé depuis `https://awscli.amazonaws.com/AWSCLIV2.msi` (HTTPS, redirections
   HTTPS seulement) ;
   dans les deux cas, le MSI est d'abord copié dans `C:\ProgramData\Kura\awscli`, que seuls
   SYSTEM et les administrateurs peuvent modifier : un programme non élevé ne peut pas le
   remplacer entre la vérification de la signature et l'installation ;
3. **signature Authenticode vérifiée** (`Get-AuthenticodeSignature` du module système, profil et
   modules de l'utilisateur ignorés : chaîne de certificats, horodatage) : état `Valid` et organisation du signataire parmi `Amazon.com Services
   LLC`, `Amazon Web Services, Inc.`, `Amazon.com, Inc.` ; sinon rien n'est installé ;
4. `msiexec /i … /qn /norestart`, journal dans `C:\ProgramData\Kura\awscli\awscli-install.log`,
   puis `aws --version`.

Une CLI AWS v1 trouvée dans le PATH est refusée (la désinstaller, ou renseigner `profile.aws_cli`) ;
une CLI présente qui ne répond pas est signalée, et `validate` tranche.

Un autre chemin dans `profile.aws_cli` n'est jamais installé automatiquement. L'agent cherche
aussi `aws.exe` dans `C:\Program Files\Amazon\AWSCLIV2` quand il n'est pas dans le PATH (cas d'une
CLI tout juste installée, avant que les processus voient le nouveau PATH).

### Installer depuis un paquet de la console

La console prépare, pour un bucket, la configuration du poste et un **paquet d'installation**
`.kura` (configuration, clé du poste et autres secrets, chiffrés comme pour `import`), qu'elle remet
dans un kit zip avec `kura-agent.exe`, `kura-agentw.exe` et `AWSCLIV2.msi`. Sur le poste, kit
décompressé, depuis une console d'administration :

```bat
kura-agent.exe install --package acme-srv01.kura
```

`install` demande le mot de passe affiché par la console (trois essais), refuse un paquet expiré,
un paquet de clé seule (à passer par `import`) et une configuration dont le bucket ou
`profile.aws_credentials` ne correspond pas au paquet ; `--package` et `--config` s'excluent. Il installe ensuite la CLI AWS si besoin (le
MSI du kit est pris en premier), range les secrets du paquet dans le coffre (ils remplacent ceux de
même nom ; les anciennes valeurs sont remises si l'installation échoue ensuite), puis suit les
étapes ci-dessus. La configuration du paquet est installée telle qu'elle a été vérifiée, sans
passer par un fichier temporaire. Le paquet est supprimé une fois l'installation réussie
(`--keep` pour le garder). Une fois la première sauvegarde réussie, confirmer l'installation de la
clé dans la console.

### Migrer depuis un script planifié

Installer avec un horaire décalé de l'ancienne tâche, vérifier la simulation (les fichiers étant déjà
dans le bucket, peu d'envois et aucune suppression massive), laisser tourner une nuit les deux
mécanismes et comparer, puis désactiver et supprimer l'ancienne tâche et son script.

### Désinstaller

```bat
kura-agent uninstall              :: tâche, icône, C:\Program Files\Kura
kura-agent uninstall --purge      :: efface aussi C:\ProgramData\Kura (configuration, secrets, journaux)
```

`uninstall` refuse de s'exécuter pendant une sauvegarde.

### Pourquoi SYSTEM

Jusqu'à la v0.4.1, la tâche tournait sous un compte dédié `svc-kura`, administrateur, sans mot de
passe stocké (mode S4U). Sur un poste Windows 11, Windows a refusé toute tâche S4U pour un autre compte
(« Accès refusé », même en autorisations limitées), alors qu'une tâche sous SYSTEM est acceptée.
`svc-kura` étant administrateur, il n'apportait pas de séparation réelle face à SYSTEM ; SYSTEM évite
en plus tout compte et tout mot de passe à gérer.

Une source sur un partage réseau (`\\serveur\partage`) serait lue avec le compte de la machine :
non prise en charge.

## L'icône (étape 5)

`kura-agentw.exe tray` tourne dans la session de chaque compte de `tray.users`. Elle ne copie rien et
n'a aucun droit particulier : si on la quitte, la sauvegarde planifiée continue seule.

| Icône | État | Info-bulle (exemple) |
| --- | --- | --- |
| nuage + coche verte | OK | Dernière sauvegarde réussie le 24/10 22:47 |
| nuage + flèche bleue (animée) | En cours | Sauvegarde en cours : Données serveur (1/2) |
| nuage + point d'exclamation orange | Avertissement | 24/10 22:47 : Outlook alice – 1 fichier(s) ignoré(s) |
| nuage + croix rouge | Erreur, garde-fou, configuration invalide | Échec le 24/10 22:47 : … |
| nuage gris + pause | Suspendu | Planification suspendue jusqu'au 27/10 08:00 |
| nuage + horloge orange | Retard | Aucune sauvegarde réussie depuis plus de 2 jours (`tray.late_after`, 48h par défaut) |

Menu (clic gauche ou droit) : dernière sauvegarde réussie et prochaine exécution ; **Sauvegarder
maintenant** (lance la tâche `\Kura\Sync`, comptes de `tray.allow_run_now`) ; **Annuler la
sauvegarde** pendant une exécution (mêmes comptes) ; **Suspendre** 1 h ou jusqu'à demain 08:00, puis
Reprendre (invite UAC : la suspension s'écrit dans `C:\ProgramData\Kura`) ; ouvrir les journaux ou la
configuration ; vérifier la configuration ; à propos ; quitter.

Notifications : fin en erreur, garde-fou ou avertissement selon `notify.toast_on` (un clic ouvre le
journal de l'exécution), retard au démarrage puis une fois par jour.

**Préavis Outlook.** Quand une exécution doit fermer Outlook, `run` demande à l'icône de la session
où il tourne d'afficher une fenêtre au premier plan avec un compte à rebours de `notice_seconds` et
deux boutons, *Fermer maintenant* et *Reporter de 1 h* (`allow_postpone`, au plus `max` fois).
À l'échéance ou sur *Fermer maintenant*, l'icône ferme Outlook proprement (comme un clic sur la croix)
et attend `graceful_timeout` ; ce qui reste ouvert est alors arrêté de force (`force: true`). Après les
jobs, l'icône relance Outlook dans la session (post-tâche `start_process`), même si la sauvegarde a
été annulée. Sans icône dans la session, ou sans réponse, `run` garde le comportement d'avant :
attente du préavis puis arrêt forcé, sans relance.

Sécurité : `run` et l'icône dialoguent par le canal `\\.\pipe\Kura-<n° de session>`, ouvert par
l'icône et accessible à son seul utilisateur, à SYSTEM et aux Administrateurs. `run` vérifie que le
canal est servi depuis la bonne session par `kura-agentw.exe` de son propre dossier, et s'y connecte
sans que l'icône puisse emprunter son identité. Une annulation n'est acceptée que d'une session dont
le compte figure dans `tray.allow_run_now`.

**Suspension** (aussi en ligne de commande, console d'administration) :
```bat
kura-agent suspend 4h          :: ou tomorrow (demain 08:00), 31 jours au plus
kura-agent suspend off         :: reprise
```
Pendant une suspension, la tâche (`run --scheduled`) se termine aussitôt sans rien faire ; un
`kura-agent run` lancé à la main n'est pas concerné. healthchecks signalera l'absence de signal.

Pendant les essais : `run` ne dialogue qu'avec les icônes lancées depuis **son propre dossier**.
Lancé depuis le dossier du zip, il ne joint pas les icônes installées dans `C:\Program Files\Kura`
(préavis par simple attente, pas de relance d'Outlook) et le signale au démarrage. Pour essayer le
préavis, lancer `"C:\Program Files\Kura\kura-agent.exe" run` ou `schtasks /Run /TN \Kura\Sync`.

Les préavis s'affichent un par un ; celui d'une exécution arrêtée en cours de route se ferme dès
que la connexion avec elle est perdue.

Journal de l'icône : `%LOCALAPPDATA%\Kura\tray.log`.

## Bilan de chaque exécution (étape 3)

Chaque exécution réelle peut envoyer son bilan à **une** destination, réglée dans `notify.report`.
Une simulation (`--dry-run`) n'envoie jamais rien. Une destination injoignable ne fait jamais
échouer la sauvegarde : l'erreur est journalisée dans `run.log`.

### healthchecks.io (ou instance auto-hébergée)

```yaml
notify:
  report:
    type: healthchecks
    url: https://hc-ping.com/<uuid-du-check>
```

| Moment | Appel |
| --- | --- |
| Début de l'exécution | `<url>/start?rid=…` (mesure de la durée, détection d'un blocage) |
| Fin, tous les jobs réussis (OK ou fichiers ignorés) | `<url>?rid=…` |
| Fin en erreur, garde-fou, job sauté, annulation | `<url>/fail?rid=…` |

- Le même `rid` relie le début et la fin. Le corps de la fin contient le résumé des jobs, visible
  dans le journal du check (10 ko au plus).
- healthchecks alerte aussi quand **aucun** signal n'arrive à l'heure prévue : serveur éteint,
  tâche désactivée. Régler la période et le délai de grâce du check sur la planification.
- Une exécution partielle (`--job`) ne touche pas au check : ni début, ni fin.
- `kura-agent validate` envoie un signal `<url>/log`, qui s'inscrit au journal du check sans changer
  son état.

### Webhook générique (Make, n8n…)

```yaml
notify:
  report:
    type: webhook
    url: https://hook.example.com/kura
    token: credential:webhook   # facultatif
    hmac: true                  # facultatif, demande un token
```

L'agent envoie en `POST` un bilan JSON à la fin de chaque exécution réelle :

```json
{
  "event": "run_finished",
  "host": "SRV01",
  "profile": "acme-srv01",
  "bucket": "acme-srv01",
  "run_id": "2026-10-24_2230",
  "result": "ok",
  "success": true,
  "exit_code": 0,
  "partial": false,
  "started_at": "2026-10-24T22:30:02+02:00",
  "ended_at": "2026-10-24T22:47:15+02:00",
  "last_success": "2026-10-24T22:47:15+02:00",
  "agent_version": "0.2.0",
  "sent_at": "2026-10-24T20:47:16Z",
  "jobs": [
    { "id": "data", "result": "ok", "uploaded": 214, "deleted": 3, "exit_code": 0 },
    { "id": "outlook", "result": "warning", "uploaded": 2, "deleted": 0, "skipped": 1, "exit_code": 2,
      "message": "1 fichier(s) ignoré(s), souvent verrouillés ; voir outlook.log" }
  ]
}
```

- `success` est vrai quand tous les jobs configurés ont tourné sans erreur ; `partial` quand
  l'exécution a été lancée avec `--job`.
- Avec `token`, l'en-tête `X-Kura-Token` porte le jeton. Avec `hmac: true`, s'ajoutent
  `X-Kura-Timestamp` et `X-Kura-Signature: sha256=<hex>`, HMAC-SHA256 de
  `<timestamp>.<corps>` avec le jeton : le broker peut vérifier l'origine du bilan et refuser un
  bilan rejoué. Jeton, URL et noms de fichiers ne figurent jamais dans le corps.
- 3 essais (puis 5 s, puis 15 s d'attente) ; en cas d'échec, le bilan est gardé dans
  `C:\ProgramData\Kura\pending\` et renvoyé, dans l'ordre, avant le suivant (20 au plus).
- Côté broker, prévoir aussi un contrôle quotidien « aucun bilan réussi depuis 48 h », que l'agent
  ne peut pas faire lui-même quand le serveur est éteint.

### Secrets : `kura-agent credentials`

`profile.aws_credentials`, `notify.report.url` et `notify.report.token` acceptent une référence
`credential:<nom>` vers le coffre de l'agent, pour ne laisser dans `config.yaml` ni clé d'accès, ni
jeton, ni URL de ping (qui suffit à envoyer un faux signal au check). `install` demande ceux qui
manquent ; pour en changer un ensuite, depuis une console d'administration :

```bat
kura-agent credentials set-aws wasabi            :: identifiant puis clé secrète Wasabi
kura-agent credentials set hc-url
kura-agent credentials set webhook --from-env WEBHOOK_TOKEN
kura-agent credentials list
kura-agent credentials delete webhook
```

Sous Windows, chaque secret est chiffré par DPAPI dans la portée de la machine (illisible copié
ailleurs) et rangé dans `C:\ProgramData\Kura\credentials\`, dont l'ACL n'autorise que SYSTEM et les
Administrateurs : la tâche (SYSTEM) et une console d'administration lisent les mêmes secrets, un
utilisateur ordinaire aucun. Les clés Wasabi passent à `aws.exe` par ses variables d'environnement,
jamais par la ligne de commande ni les journaux.

### Clé remise par la console : `kura-agent import`

La console [kura-ctl](https://github.com/E-C-G/kura-ctl) crée la clé du poste (limitée à son bucket,
sans droit de supprimer une version) et la remet dans un fichier `.kura` chiffré (AES-256-GCM,
clé dérivée par PBKDF2-SHA256), téléchargeable une seule fois et valable 72 h. Elle affiche une
seule fois son mot de passe, de la forme `K7QM-2XRT-9HWB-P4NC-VJ3F` (casse et tirets indifférents).
Sur le poste, depuis une console d'administration :

```bat
kura-agent import C:\Users\alice\Downloads\acme-srv01.kura
```

L'import vérifie que le paquet vise le bucket de la configuration installée, **essaie la nouvelle
clé auprès de Wasabi** (`head-bucket`) puis seulement la range dans le coffre sous le nom prévu
(`wasabi` par défaut) : une clé refusée ne remplace jamais celle qui sauvegarde. Si ce nom n'est
pas celui de `profile.aws_credentials`, l'import est refusé (`--force` pour passer outre) : la clé
serait rangée sans jamais servir. Le fichier est ensuite supprimé (`--keep` pour le garder). Une fois la sauvegarde
suivante réussie, confirmer l'installation dans la console, qui désactive alors l'ancienne clé.

Le format est défini dans `pkg/pack`, importé par la console : un seul code fait foi des deux côtés.

## Ce que fait une exécution

1. Prend le verrou `Global\Kura-Run` (une seule exécution à la fois, code 4 sinon).
2. Les jobs sans `depends_on_pre_task` démarrent tout de suite, en même temps que la pré-tâche ; ceux
   qui en dépendent passent après elle (un report d'Outlook ne retarde donc qu'eux).
   Si un job qui en dépend est lancé : cherche `outlook.exe` dans toutes les sessions et, pour chaque
   session, fait afficher le préavis par l'icône puis fermer Outlook proprement (sinon attend le
   préavis) ; cherche de nouveau et arrête de force ce qui tourne encore (`force: true`), après avoir
   revérifié le nom de chaque processus. S'il reste ouvert, les jobs marqués `depends_on_pre_task`
   sont sautés (avertissement). Après les jobs, l'icône relance Outlook là où il a été fermé.
3. Pour chaque job avec `delete: true`, **avant chaque tentative** de synchronisation : vérifie la
   source, compte ses fichiers, simule (`--dryrun`) et bloque le job si la source semble vide ou si les
   suppressions prévues dépassent les seuils. Un job bloqué ne touche pas au bucket.
   - Tout job avec `delete: true` exige au moins 1 fichier dans sa source, même avec une section
     `guard` partielle.
   - `--force-deletes`, toujours avec un seul `--job`, lève les plafonds de suppressions (`max_deletes`,
     `max_delete_ratio`) pour ce job et cette exécution. Il ne lève jamais `min_source_files` : une
     source vide ou démontée ne se force pas ; si c'est voulu, baisser le seuil dans la configuration.
4. Lance `aws s3 sync` ; en cas d'erreur passagère (codes 1, 254, 255), 2 nouvelles tentatives à 5 min
   d'intervalle, chacune précédée du contrôle du point 3.
5. Écrit `status.json`, ajoute une ligne à `history.jsonl` (500 dernières), purge les journaux de plus
   de `retention_days`.

Dans `status.json`, `last_success` n'avance que si **tous** les jobs configurés ont réussi (OK ou
avertissement) ; un job sauté ou une exécution partielle (`--job`) ne compte pas.
`job_last_success` donne la dernière réussite de chaque job.

Un job dont la source ne se résout pas sur la machine (`${PROFILE:bob}` d'un compte supprimé, variable
absente) finit seul en erreur : les autres jobs s'exécutent. `validate` et `install` le signalent.

Coût en temps : chaque tentative d'un job avec `delete: true` parcourt la source puis simule
(`--dryrun`, qui liste tout le préfixe du bucket) avant de synchroniser. Sur un gros volume, mesurer la
durée d'une nuit dans `run.log` et ajuster `max_duration`.

La déconnexion d'un utilisateur du serveur n'interrompt pas la sauvegarde ; Ctrl+C l'annule (code 6).
Dépasser `max_duration` termine l'exécution en erreur (code 2).

### Sommes de contrôle et Wasabi

Depuis la version 2.23 (janvier 2025), la CLI AWS calcule par défaut une somme de contrôle
CRC et l'envoie **en fin de requête** (trailer). Wasabi ne la lit pas : les fichiers envoyés en
plusieurs parties (plus de 8 Mo) échouent en `Checksum Type mismatch … expected checksum Type:
crc32, actual checksum Type: null`. L'agent lance donc toujours `aws.exe` avec
`AWS_REQUEST_CHECKSUM_CALCULATION=WHEN_REQUIRED` et `AWS_RESPONSE_CHECKSUM_VALIDATION=WHEN_REQUIRED`
(somme de contrôle seulement quand l'opération l'exige, comme avant 2.23), et ne transmet plus
`profile.checksum_algorithm`, accepté mais ignoré. L'intégrité du transfert reste assurée par TLS.

## Codes de sortie de `run`

| Code | Signification |
| --- | --- |
| 0 | Tous les jobs OK |
| 1 | Avertissement (fichiers ignorés car verrouillés, job sauté car Outlook ouvert) |
| 2 | Au moins un job en erreur |
| 3 | Configuration invalide ou AWS CLI introuvable |
| 4 | Une exécution est déjà en cours |
| 5 | Au moins un job bloqué par le garde-fou |
| 6 | Annulée |

Un fichier que la CLI signale (« Skipping file … ») alors qu'une exclusion du job l'écarte de
toute façon ne compte pas : la CLI vérifie l'existence des fichiers avant d'appliquer
`--exclude`, si bien qu'un lien symbolique cassé dans un dossier exclu (`.venv` créé sous WSL,
par exemple) la fait sortir en code 2 à chaque passage. L'agent rejoue le filtrage de la CLI
sur le chemin signalé ; seuls les fichiers réellement manqués donnent un avertissement.

## Fichiers sur la machine

```text
C:\ProgramData\Kura\
├── config.yaml
├── status.json              état courant, réécrit atomiquement
├── history.jsonl            une ligne par exécution
├── suspend.json             suspension en cours (kura-agent suspend)
├── credentials\             secrets chiffrés (administrateurs seulement)
├── pending\                 bilans webhook en attente de renvoi
└── logs\runs\2026-10-24_2230\
    ├── run.log              déroulé de l'exécution
    ├── data.log             commandes et sortie brute d'aws pour le job "data"
    └── outlook.log
```

`KURA_DATA_DIR` remplace `C:\ProgramData\Kura` (tests, développement).

## Organisation du code

```text
cmd/kura-agent/     ligne de commande (kura-agent.exe, et kura-agentw.exe compilé avec -H=windowsgui)
pkg/config/         format de configuration (importé par kura-ctl)
pkg/pack/           paquet .kura chiffré remis par kura-ctl (scellé par kura-ctl, ouvert par import et install)
internal/runner/    orchestration d'une exécution, codes de sortie
internal/awscli/    commandes aws s3 sync, lecture de leur sortie
internal/guard/     comptage des sources, seuils du garde-fou
internal/procs/     recherche et arrêt de processus toutes sessions
internal/execx/     lancement d'aws.exe sans fenêtre, arrêt garanti
internal/lock/      verrou machine
internal/state/     status.json, history.jsonl
internal/report/    bilan vers healthchecks ou webhook
internal/ipc/       dialogue run ↔ icône : messages JSON, canal nommé par session
internal/tray/      icône : modèle d'affichage (portable), interface Win32
internal/creds/     coffre de secrets (DPAPI sous Windows)
internal/platform/  fonctions dépendantes du système : ce qui est écrit pour chacun, « non pris en charge »
internal/systemd/   planification Linux : kura-sync.service et kura-sync.timer, systemctl
internal/setup/     ACL, copie des fichiers, icône au démarrage, ancien compte svc-kura (install)
internal/awsinstall/ CLI AWS absente : MSI fourni ou téléchargé, signature Amazon vérifiée, msiexec
internal/schedtask/ tâche \Kura\Sync : XML du Planificateur, schtasks
internal/paths/     emplacements sur disque
```

`internal/runner` ne dépend d'aucune interface graphique : ses tests utilisent une fausse CLI AWS
et un faux gestionnaire de processus (nuit normale, garde-fou, `--force-deletes`, fichiers verrouillés,
erreurs passagères et définitives, Outlook impossible à fermer, source absente, simulation, annulation).
