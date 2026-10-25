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
| 3 | Bilan au webhook (broker Make ou autre) | à faire |
| 4 | `install` / `uninstall` / `credentials` : compte `svc-kura`, tâche `\Kura\Sync`, DPAPI | à faire |
| 5 | Icône par session, préavis et fermeture douce d'Outlook | à faire |
| 6 | Restauration (V2) | à faire |

Commandes disponibles : `run`, `validate`, `status`, `version`.

## Compiler

Go 1.24 ou plus récent. Première fois, avec accès à Internet :

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

**Signature Authenticode** (facultative mais recommandée : sans elle, SmartScreen et certains
antivirus signalent l'exécutable). Ajouter dans *Settings → Secrets and variables → Actions* :

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
internal/paths/     emplacements sur disque
```

`internal/runner` ne dépend d'aucune interface graphique : ses tests utilisent une fausse CLI AWS
et un faux gestionnaire de processus (nuit normale, garde-fou, `--force-deletes`, fichiers verrouillés,
erreurs passagères et définitives, Outlook impossible à fermer, source absente, simulation, annulation).
