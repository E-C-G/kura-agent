# kura-agent

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
   préavis, l'arrête de force (`force: true`). S'il reste ouvert, les jobs marqués
   `depends_on_pre_task` sont sautés (avertissement).
3. Pour chaque job avec `delete: true` : compte les fichiers source, simule (`--dryrun`), bloque le job
   si la source semble vide ou si les suppressions prévues dépassent les seuils. Un job bloqué ne touche
   pas au bucket. `--force-deletes` lève le blocage pour une exécution, après vérification.
   Sans section `guard`, un job avec `delete: true` reçoit le minimum : au moins 1 fichier dans la source.
4. Lance `aws s3 sync` ; en cas d'erreur passagère (codes 1, 254, 255), 2 nouvelles tentatives à 5 min
   d'intervalle.
5. Écrit `status.json`, ajoute une ligne à `history.jsonl` (500 dernières), purge les journaux de plus
   de `retention_days`.

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
