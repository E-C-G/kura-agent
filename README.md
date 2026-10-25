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
| 4 | `install` / `uninstall` / `credentials` : compte `svc-kura`, tâche `\Kura\Sync`, DPAPI | à faire |
| 5 | Icône par session, préavis et fermeture douce d'Outlook | à faire |
| 6 | Restauration (V2) | à faire |

Commandes disponibles : `run`, `validate`, `status`, `credentials`, `version`.

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

## Mettre en service à la place d'un script planifié existant (étape 2)

En attendant `kura-agent install` (étape 4), la mise en place est manuelle, sur le serveur :

1. Copier `kura-agent.exe` dans `C:\Program Files\Kura\`.
2. Copier `configs/example.yaml` vers `C:\ProgramData\Kura\config.yaml`, puis remplacer les valeurs
   fictives (`acme-srv01`, `alice`, région, chemins) par les vôtres et adapter :
   - `profile.aws_profile` : le profil AWS que le script utilise aujourd'hui. Le script ne passe pas
     `--profile`, il utilise donc le profil `default` du compte qui exécute la tâche : mettre `default`,
     ou laisser vide.
   - `notice_seconds` : 120 s de préavis avant d'arrêter Outlook. Mettre `0` pour garder exactement le
     comportement actuel (`taskkill /F` immédiat). La fermeture douce viendra avec l'icône (étape 5).
3. Vérifier, dans une invite de commandes administrateur :
   ```bat
   "C:\Program Files\Kura\kura-agent.exe" validate
   "C:\Program Files\Kura\kura-agent.exe" run --dry-run
   ```
   La simulation ne ferme pas Outlook et ne modifie rien dans le bucket ; elle affiche les envois et
   suppressions prévus. Les fichiers étant déjà dans le bucket, les deux jobs doivent annoncer peu de chose.
4. Dans le Planificateur de tâches, remplacer l'action de la tâche existante par :
   `"C:\Program Files\Kura\kura-agent.exe" run --quiet`
   et cocher « Exécuter avec les autorisations maximales » (nécessaire pour arrêter Outlook dans la
   session de `alice` et lire son profil).
5. Le lendemain, `kura-agent status` affiche le bilan ; le code de sortie apparaît aussi dans
   l'historique du Planificateur.

Le passage au compte dédié `svc-kura` viendra avec `kura-agent install`.

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

`url` et `token` acceptent une référence `credential:<nom>` vers le coffre de l'agent, pour ne laisser
dans `config.yaml` ni jeton ni URL de ping (qui suffit à envoyer un faux signal au check).

```bat
kura-agent credentials set webhook
kura-agent credentials set hc-url --from-env HC_URL
kura-agent credentials list
kura-agent credentials delete webhook
```

Sous Windows, chaque secret est chiffré par DPAPI pour le **compte qui l'enregistre** : lancer ces
commandes sous le compte qui exécute la tâche planifiée, sinon `run` ne pourra pas le relire.
`kura-agent install` (étape 4) s'en chargera pour le compte `svc-kura`. Les fichiers sont rangés
dans `C:\ProgramData\Kura\credentials\`.

## Ce que fait une exécution

1. Prend le verrou `Global\Kura-Run` (une seule exécution à la fois, code 4 sinon).
2. Si un job qui en dépend est lancé : cherche `outlook.exe` dans toutes les sessions, attend le
   préavis, cherche de nouveau (Outlook a pu être fermé entre-temps) et arrête de force ce qui tourne
   encore (`force: true`), après avoir revérifié le nom de chaque processus. S'il reste ouvert, les jobs
   marqués `depends_on_pre_task` sont sautés (avertissement).
3. Pour chaque job avec `delete: true`, **avant chaque tentative** de synchronisation : vérifie la
   source, compte ses fichiers, simule (`--dryrun`) et bloque le job si la source semble vide ou si les
   suppressions prévues dépassent les seuils. Un job bloqué ne touche pas au bucket.
   - Tout job avec `delete: true` exige au moins 1 fichier dans sa source, même avec une section
     `guard` partielle.
   - `--force-deletes`, toujours avec `--job`, lève les plafonds de suppressions (`max_deletes`,
     `max_delete_ratio`) pour ce job et cette exécution. Il ne lève jamais `min_source_files` : une
     source vide ou démontée ne se force pas ; si c'est voulu, baisser le seuil dans la configuration.
4. Lance `aws s3 sync` ; en cas d'erreur passagère (codes 1, 254, 255), 2 nouvelles tentatives à 5 min
   d'intervalle, chacune précédée du contrôle du point 3.
5. Écrit `status.json`, ajoute une ligne à `history.jsonl` (500 dernières), purge les journaux de plus
   de `retention_days`.

Dans `status.json`, `last_success` n'avance que si **tous** les jobs configurés ont réussi (OK ou
avertissement) ; un job sauté ou une exécution partielle (`--job`) ne compte pas.
`job_last_success` donne la dernière réussite de chaque job.

La déconnexion d'un utilisateur du serveur n'interrompt pas la sauvegarde ; Ctrl+C l'annule (code 6).
Dépasser `max_duration` termine l'exécution en erreur (code 2).

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

## Fichiers sur la machine

```text
C:\ProgramData\Kura\
├── config.yaml
├── status.json              état courant, réécrit atomiquement
├── history.jsonl            une ligne par exécution
├── credentials\             secrets chiffrés (kura-agent credentials)
├── pending\                 bilans webhook en attente de renvoi
└── logs\runs\2026-10-24_2230\
    ├── run.log              déroulé de l'exécution
    ├── data.log             commandes et sortie brute d'aws pour le job "data"
    └── outlook.log
```

`KURA_DATA_DIR` remplace `C:\ProgramData\Kura` (tests, développement).

## Organisation du code

```text
cmd/kura-agent/     ligne de commande
pkg/config/         format de configuration (importé par kura-ctl)
internal/runner/    orchestration d'une exécution, codes de sortie
internal/awscli/    commandes aws s3 sync, lecture de leur sortie
internal/guard/     comptage des sources, seuils du garde-fou
internal/procs/     recherche et arrêt de processus toutes sessions
internal/execx/     lancement d'aws.exe sans fenêtre, arrêt garanti
internal/lock/      verrou machine
internal/state/     status.json, history.jsonl
internal/report/    bilan vers healthchecks ou webhook
internal/creds/     coffre de secrets (DPAPI sous Windows)
internal/paths/     emplacements sur disque
```

`internal/runner` ne dépend d'aucune interface graphique : ses tests utilisent une fausse CLI AWS
et un faux gestionnaire de processus (nuit normale, garde-fou, `--force-deletes`, fichiers verrouillés,
erreurs passagères et définitives, Outlook impossible à fermer, source absente, simulation, annulation).
