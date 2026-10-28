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
| 6 | Restauration : historique, état à une date (lot 1), vers un dossier à part, journal, bilan (lot 2), en place (lot 3), depuis l'icône pour ses propres fichiers (lot 4) | **fait** |
| 7 | `self-update` : recherche et vérification de l'attestation GitHub, installation, retour arrière, icône | **fait** |
| 8 | Préparations avant les jobs : archive zip (lot 1) ; exports de bases (lot 2), chiffrement gpg et age (lot 3) | lots 1 et 2 **faits** |

Commandes disponibles : `run`, `validate`, `status`, `credentials`, `import`, `install`, `uninstall`,
`suspend`, `tray`, `self-update`, `restore`, `version`.

## Systèmes pris en charge

Windows (10, 11, Server) et Linux (Ubuntu 24.04 ; Fedora ensuite) ; macOS (13 Ventura ou plus
récent, Apple silicon et Intel) est en cours : chaque release en contient les archives
(`kura-agent_<version>_darwin_amd64.tar.gz` pour Intel, `darwin_arm64` pour Apple silicon), non
signées, pour essai (`xattr -d com.apple.quarantine kura-agent` après téléchargement). Le cœur de l'agent (configuration, garde-fou, synchronisation, bilan,
paquet `.kura`, état) est commun et testé partout ; les fonctions qui dépendent du système sont
séparées, un fichier par système, et recensées dans `internal/platform` :

| Fonction | Paquets | Windows | Linux, macOS |
| --- | --- | --- | --- |
| planification | `schedtask`, `systemd`, `launchd` | oui | Linux : oui (timer systemd) ; macOS : oui (démons launchd sous root, rattrapage au démarrage) |
| coffre des secrets | `creds` | oui | Linux : oui (systemd-creds) ; macOS : oui (AES-256-GCM, clé dans le trousseau Système) |
| installation | `setup`, `install` | oui | Linux : oui ; macOS : oui (root ; accès complet au disque à accorder à la main) |
| CLI AWS automatique | `awsinstall` | oui | Linux : oui (zip signé PGP) ; macOS : oui (`AWSCLIV2.pkg` signé par l'équipe Apple d'Amazon, notarisé) |
| icône et canal vers l'icône | `tray`, `ipc` | oui | non pris en charge |
| fermeture d'applications | `procs` (pré-tâches) | oui | non pris en charge |
| export SQL Server | `prepare` (`db_export`, `engine: sqlserver`) | oui | non pris en charge (PostgreSQL, MySQL : oui) |

Hors Windows, une commande qui a besoin d'une fonction absente s'arrête tout de suite et dit ce
qui manque. Sous Linux, toutes les commandes fonctionnent sauf `tray` (avec `sudo` pour le coffre
et l'installation) ; une configuration avec `pre_tasks` ou `post_tasks` y est refusée (fermeture
d'applications et icône à venir). Sous macOS (13 Ventura ou plus récent), toutes les commandes fonctionnent sauf `tray`, avec
`sudo` pour le coffre et l'installation ; mêmes refus de `pre_tasks` et `post_tasks`. Avant la
première sauvegarde, accorder l'accès complet au disque à `/usr/local/bin/kura-agent`
(Réglages Système > Confidentialité et sécurité) : sans lui, Documents, Bureau et Mail restent
illisibles, même pour root. Hors système pris en charge :

```text
$ kura-agent install
kura-agent install : non pris en charge sous freebsd dans cette version (manque : installation,
planification, coffre des secrets, installation de la CLI AWS).
```

### Installer sous Linux (Ubuntu 24.04)

Le paquet Debian de la release (`kura-agent_<version>_amd64.deb`, ou `arm64`) installe
l'exécutable dans `/usr/bin` et crée le compte de service ; il ne configure rien :

```bash
sudo apt install ./kura-agent_0.7.0_amd64.deb
sudo kura-agent install --package <fichier>.kura   # ou --config config.yaml
```

Le paquet dépend de `systemd` (254 ou plus récent, pour `ImportCredential`), `gpgv` et
`ca-certificates` ; exemple de configuration dans `/usr/share/doc/kura-agent/examples/config.yaml`.
`apt remove` retire aussi le timer et le service (`kura-agent uninstall`) ; `apt purge` efface en
plus configuration, état, journaux et secrets `kura.*` (le compte `kura-agent` reste). Construction :
`make deb` (ou `scripts/build-deb.sh VERSION ARCH BINAIRE DOSSIER`) ; la CI installe le paquet sur
un vrai Ubuntu, le met en place (compte, timer) puis le purge à chaque commit.

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
`kura-sync.service` (`kura-agent run --scheduled`, déroulé aussi dans `journalctl -u kura-sync`). Les deux unités sont générées depuis
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
La CLI du paquet snap (`/snap/bin/aws`) est écartée : `snap-confine` exige des droits que le
service n'a pas (il ne garde que la lecture du disque), elle échoue sous le service alors qu'elle
marche avec `sudo`. `install` installe alors la version officielle, qui passe avant dans le PATH.

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
   - protège `C:\ProgramData\Kura` (Utilisateurs en lecture seule, coffre réservé aux administrateurs,
     propriétaire : groupe Administrateurs). Un dossier créé à l'avance par un compte non
     administrateur (qui en resterait propriétaire et pourrait se rendre l'accès) ou une jonction
     à sa place sont refusés, avec la commande pour les supprimer ; même contrôle pour
     `credentials\` et `awscli\` ;
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
une CLI présente qui ne répond pas est signalée, et `validate` tranche. Une CLI v2 antérieure à
la version minimale (`MinCLIVersion`, 2.32.0) est mise à jour ; un paquet authentiquement signé
mais plus ancien, posé à côté du paquet `.kura`, est refusé (versions aux failles connues).

Un autre chemin dans `profile.aws_cli` n'est jamais installé automatiquement. L'agent cherche
d'abord `aws.exe` dans `C:\Program Files\Amazon\AWSCLIV2` (`/usr/local/bin/aws` sous Linux et
macOS), puis dans le PATH : un dossier du PATH modifiable par un utilisateur ne peut pas lui
substituer un autre `aws`.

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
kura-agent uninstall              :: tâches \Kura\Sync et \Kura\Restore, icône, C:\Program Files\Kura
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
configuration ; vérifier la configuration ; à propos ; quitter. **Restaurer mes fichiers…** pour tous
les comptes de `tray.users` (voir ci-dessous).

**Restaurer mes fichiers.** Chaque compte de l'icône restaure ses propres fichiers, sans droits
d'administrateur, dans une fenêtre :

- un dossier (raccourcis : les dossiers sauvegardés où le compte peut écrire, dont son profil ;
  ou *Parcourir…*), une date (calendrier borné aux 90 derniers jours ; aujourd'hui = dernière
  sauvegarde), *Afficher* ;
- la liste des fichiers du dossier à cette date (sous-dossiers compris) : nom, taille, date de la
  sauvegarde, « supprimé le… », « modifié depuis » ; cases à cocher, *Tout cocher*, *Tout
  décocher* ; l'historique du fichier sélectionné en dessous ;
- *Restaurer* : dans un nouveau dossier « Restauration Kura AAAA-MM-JJ HHhMM » créé à côté du
  dossier choisi, ou à leur place pour les seuls fichiers supprimés ; jamais d'écrasement ;
- avancement dans la fenêtre, notification à la fin (un clic ouvre le dossier), bouton *Ouvrir le
  dossier*.

L'icône ouvre pour cela un second canal (`\\.\pipe\KuraRestore-<session>-…`, publié dans
`HKCU\Software\Kura\RestorePipe` le temps de la fenêtre) et lance la tâche `\Kura\Restore`
(SYSTEM, créée par `install`, lançable par les comptes de `tray.users`, une instance pour toutes les
sessions). La tâche (`kura-agent restore --serve`) joint la fenêtre (exécutable et session vérifiés
comme pour `run`) ; la fenêtre n'accepte qu'un client SYSTEM. Tout est vérifié par la tâche, avec le
jeton du processus de l'icône (pris lors de sa vérification) : le compte doit figurer dans
`tray.users` ; un fichier n'est listé que si le compte peut lister **son** dossier et y écrire
(contrôle d'accès de Windows fait avec son jeton, dossier par dossier : le sous-dossier privé d'un
dossier commun n'apparaît pas, ni le profil d'un autre compte), et, s'il est encore présent, le lire ;
un dossier disparu n'est restaurable depuis l'icône que dans le profil du compte (ailleurs, il a pu
appartenir à un autre compte : restauration par l'administrateur, en ligne de commande) ; hors du
profil, seuls les fichiers encore présents sont proposés (versions antérieures) : un fichier
supprimé d'un dossier commun a pu appartenir à un autre compte et rien sur le disque ne le prouve,
il se restaure par l'administrateur ; seuls les dossiers des disques du poste sont proposés (pas de
partage réseau) ; les fichiers sont écrits **en son nom** (emprunt
d'identité), donc à lui et seulement là où il pourrait écrire lui-même, et jamais à la place d'un
fichier existant, même apparu pendant la restauration. Jamais servis depuis l'icône, quoi que
contienne le bucket : les préfixes en quarantaine, tout chemin sous un dossier de l'agent
(données, coffre, configuration, journaux, programme), et les fichiers du coffre et paquets de
la console (`*.secret`, `*.kura`) où qu'ils se trouvent. Le compte ne voit jamais la clé. La date est
bornée à 90 jours par la tâche elle-même. Pas de verrou de la sauvegarde depuis l'icône (rien n'y
est remplacé ; un compte standard ne doit pas pouvoir bloquer la sauvegarde). Le dossier
« Restauration Kura … » se trouve dans une source sauvegardée : il partira à la sauvegarde
suivante, le supprimer une fois les fichiers remis en place. Journal et bilan comme en ligne de
commande, avec le compte qui a restauré ; journal de la tâche : `restore-tray.log` dans le dossier
des journaux. La tâche s'arrête 90 s après la fermeture de la dernière fenêtre, ou au bout de 30 min
sans demande.

Mise à jour, pour les seuls membres du groupe Administrateurs du poste (groupes imbriqués
compris ; sous UAC, le compte compte même si ses droits ne sont pas encore élevés) : **Mettre à
jour vers X…** quand la recherche planifiée a noté une version plus récente (grisé pendant une
sauvegarde), sinon **Rechercher une mise à jour** (`self-update --check`, sans élévation). Après
confirmation, l'icône lance `kura-agentw.exe self-update --yes --from-tray` avec élévation
(invite UAC) ; ce processus annonce l'avancement à toutes les icônes ouvertes (infobulle), la fin
(notification en cas d'échec) et, après une mise à jour réussie, leur demande de se relancer
depuis la nouvelle version, qui le notifie. Une icône n'accepte cette relance que d'un processus
élevé ou de SYSTEM. Un compte standard ne voit rien de la mise à jour.

Notifications : fin en erreur, garde-fou ou avertissement selon `notify.toast_on` (un clic ouvre le
journal de l'exécution), retard au démarrage puis une fois par jour.

**Préavis Outlook.** Quand une exécution doit fermer Outlook, `run` demande à l'icône de la session
où il tourne d'afficher une fenêtre au premier plan avec un compte à rebours de `notice_seconds` et
deux boutons, *Fermer maintenant* et *Reporter de 1 h* (`allow_postpone`, au plus `max` fois).
À l'échéance ou sur *Fermer maintenant*, l'icône ferme Outlook proprement (comme un clic sur la croix)
et attend `graceful_timeout` ; ce qui reste ouvert est alors arrêté de force (`force: true`). L'icône
relance Outlook dans la session (post-tâche `start_process`) dès que plus rien n'a besoin qu'il
reste fermé : jobs marqués `depends_on_pre_task` terminés, et source des préparations qui en
dépendent copiée (l'archive et son envoi se font ensuite, Outlook rouvert) ; au plus tard à la fin
de l'exécution, même si la sauvegarde a été annulée. Sans icône dans la session, ou sans réponse, `run` garde le comportement d'avant :
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
- `prepares` (seulement si des préparations ont tourné) : `id`, `type`, `result`, `files`, `size`
  (octets), `message`.
- Avec `token` seul, l'en-tête `X-Kura-Token` porte le jeton. Avec `hmac: true`, le jeton ne
  voyage plus : `X-Kura-Timestamp` et `X-Kura-Signature: sha256=<hex>`, HMAC-SHA256 de
  `<timestamp>.<corps>` avec le jeton, permettent au broker de vérifier l'origine du bilan et de
  refuser un bilan rejoué. Jeton, URL et noms de fichiers ne figurent jamais dans le corps.
- Aucune redirection n'est suivie (le jeton et l'URL de ping ne partent jamais ailleurs) ;
  l'adresse lue dans le coffre doit être en `https://`, comme dans la configuration.
- 3 essais (puis 5 s, puis 15 s d'attente) ; en cas d'échec, le bilan est gardé dans
  `C:\ProgramData\Kura\pending\` et renvoyé, dans l'ordre, avant le suivant (20 au plus).
- Côté broker, prévoir aussi un contrôle quotidien « aucun bilan réussi depuis 48 h », que l'agent
  ne peut pas faire lui-même quand le serveur est éteint.

Après chaque restauration (`kura-agent restore … --to`), le webhook reçoit un message à part,
sans effet sur le suivi des sauvegardes ; healthchecks reçoit le même résumé sur `<url>/log`
(journal du check, ni succès ni échec). Le message porte les chemins demandés, pas la liste des
fichiers ; il n'est pas mis en attente s'il ne passe pas.

```json
{
  "event": "restore_finished",
  "type": "restore",
  "host": "SRV01",
  "profile": "acme-srv01",
  "bucket": "acme-srv01",
  "agent_version": "0.13.0",
  "sent_at": "2026-10-28T10:12:41Z",
  "account": "SRV01\\admin",
  "mode": "to",
  "paths": ["D:\\Projets\\Clients"],
  "to": "E:\\Restauration",
  "at": "2026-10-20T23:59:59+02:00",
  "files": 37,
  "replaced": 0,
  "bytes": 441344,
  "skipped": 0,
  "ignored": 0,
  "failed": 0,
  "result": "ok",
  "exit_code": 0,
  "log": "C:\\ProgramData\\Kura\\logs\\restore-20261028-111203.log",
  "started_at": "2026-10-28T11:12:03+01:00",
  "ended_at": "2026-10-28T11:12:40+01:00"
}
```

`mode` : `to` (dossier à part), `in_place` ou `replace` (`to` vaut alors `en place`) ;
`result` : `ok`, `partial` (des fichiers en échec) ou `interrupted` ; `at` est absent pour
l'état de la dernière sauvegarde.

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

1. Prend le verrou `run.lock` du dossier de données (une seule exécution à la fois, code 4
   sinon). Seuls l'agent et les administrateurs peuvent ouvrir ce fichier : un utilisateur ne
   peut pas le tenir pour bloquer les sauvegardes.
2. Les jobs sans `depends_on_pre_task` démarrent tout de suite, en même temps que la pré-tâche ; ceux
   qui en dépendent passent après elle (un report d'Outlook ne retarde donc qu'eux).
   Si un job qui en dépend est lancé : cherche `outlook.exe` dans toutes les sessions et, pour chaque
   session, fait afficher le préavis par l'icône puis fermer Outlook proprement (sinon attend le
   préavis) ; cherche de nouveau et arrête de force ce qui tourne encore (`force: true`), après avoir
   revérifié le nom de chaque processus. S'il reste ouvert, les jobs marqués `depends_on_pre_task`
   sont sautés (avertissement). L'icône relance Outlook là où il a été fermé dès que les jobs qui
   en dépendent sont finis et que les préparations qui en dépendent ont copié leur source.
3. Chaque source doit être un vrai dossier : une source qui est un lien symbolique ou une
   jonction est refusée (indiquer le dossier réel). Les liens **dans** une source ne sont jamais
   suivis : un utilisateur qui écrit dans une source ne doit pas pouvoir y faire entrer un
   fichier qu'il ne sait pas lire (coffre de l'agent, profil d'un autre compte). Sous Windows,
   la CLI AWS suit les jonctions malgré `--no-follow-symlinks` : avant chaque synchronisation,
   l'agent parcourt donc la source (sans rien suivre), relève chaque lien symbolique et chaque
   jonction (hors dossiers exclus) et les passe à `aws` en `--exclude` (journal du job : « lien(s)
   ou jonction(s) non suivi(s), exclu(s) »). Pendant la synchronisation, chaque fichier envoyé est
   contrôlé : s'il est passé par un lien apparu après le parcours, le job finit en erreur et le
   préfixe concerné est noté dans `quarantine.json` (dossier des données) : aucune restauration
   ne le sert (ligne de commande comprise, sauf `--include-quarantined` après examen), et
   `kura-agent status` l'affiche. Une quarantaine illisible n'est jamais réécrite et bloque
   les restaurations jusqu'à correction.
   Pour chaque job avec `delete: true`, **avant chaque tentative** de synchronisation : vérifie la
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

### Coupures réseau

Une coupure de quelques dizaines de secondes au milieu du listage d'un gros dossier faisait
abandonner `aws` (« Could not connect to the endpoint URL ») après ses 3 essais par défaut, et
toute la passe était à refaire. L'agent lance `aws.exe` avec `AWS_RETRY_MODE=standard` et
`AWS_MAX_ATTEMPTS=10` : chaque requête est réessayée, avec une pause croissante plafonnée à
20 s, pendant environ 2 minutes. Au-delà, l'agent relance la commande entière (2 nouvelles
tentatives à 5 min d'intervalle, voir plus haut).

## Préparations : archive zip et export de bases avant les jobs

Une préparation écrit, avant les jobs, un fichier que la synchronisation seule ne sait pas
sauvegarder proprement ; un job ordinaire l'envoie ensuite en prenant `${STAGING:<id>}` pour
source. Cette version fait les archives zip (un dossier de dizaines de milliers de petits
fichiers, ou à sauvegarder d'un bloc) et les exports de bases (voir plus bas) ; le chiffrement
suivra.

```yaml
tools:                             # outils installés sur le poste, chemins absolus
  zip: "C:/Program Files/7-Zip/7z.exe"   # ou /usr/bin/zip (Info-ZIP)

prepare:
  - id: courrier
    type: zip
    source: "D:/Courrier"
    exclude: ["*.tmp"]
    level: deflate                 # deflate (défaut) | store
    on_locked: fail                # fail (défaut) | skip : fichier illisible laissé de côté, avertissement
    timeout: 1h                    # défaut 1 h
    depends_on_pre_task: outlook.exe   # facultatif (Windows) : attendre la fermeture d'Outlook

jobs:
  - id: courrier
    name: Courrier
    source: "${STAGING:courrier}"
    destination: COURRIER
    delete: true
```

- **Outils.** L'agent n'installe ni ne cherche aucun outil : `tools.zip` donne le chemin absolu
  d'Info-ZIP `zip` 3.0 ou plus (Linux, macOS) ou de 7-Zip `7z` 16.02 ou plus (seul accepté sous
  Windows). À l'installation, dans `validate` puis à chaque exécution, l'outil doit exister, donner
  une version reconnue, et ni lui ni ses dossiers parents ne doivent être modifiables par un autre
  compte que les administrateurs. Windows : propriétaire et droits de chaque dossier, tout le
  dossier de l'outil compris (7-Zip y charge `7z.dll`, `Codecs`, `Formats`), jonctions et entrées
  de droits non standard refusées, outil posé à la racine du volume refusé. Linux, macOS : root,
  sans écriture pour le groupe ni les autres, sans ACL étendue (Linux). Sinon un utilisateur ferait
  exécuter son code par l'agent. L'outil tourne avec un environnement réduit (aucune variable de
  l'agent).
- **Archive.** L'agent copie d'abord les fichiers retenus dans la zone de préparation : liens et
  jonctions jamais suivis, exclusions appliquées, dossiers de l'agent laissés de côté (comparés
  par identité de fichier), chaque fichier ouvert sous la source (`os.Root`) : un dossier remplacé
  par un lien pendant la copie ne mène jamais hors d'elle. L'outil archive cette copie, que seul
  l'agent peut modifier (`zip -y -nw -@`, `7z a -tzip -snl -spd @liste`) : il ne suit donc aucun lien
  posé par un utilisateur et ne charge rien d'un dossier qu'un utilisateur contrôle. L'archive est
  relue en entier (sommes CRC) et ne doit rien contenir d'autre ; avec `on_locked: fail`, aucun
  fichier ne doit manquer (fichier ou dossier illisible, nom impossible à passer à l'outil). Prévoir
  dans le dossier de données deux fois la taille de la source le temps de l'archive.
- **Espace disque.** La zone de préparation est sur le disque du dossier de données (`C:` sous
  Windows) : l'agent y garde toujours 2 Go libres. En dessous au départ, la préparation ne commence
  pas ; en cours de copie ou d'archive (contrôle toutes les 2 s), elle s'arrête, l'outil est arrêté,
  la copie et l'archive partielle sont effacées, et la préparation échoue avec l'espace restant.
- **Outlook.** Avec `depends_on_pre_task`, Outlook reste fermé le temps de la copie de la source
  seulement : il est relancé avant l'archive et l'envoi, qui ne lisent que la copie (sauf si un
  autre job en dépend encore).
- **Nom stable.** Le résultat s'appelle toujours `<id>.zip` : l'historique vient des versions du
  bucket (90 jours) et `restore --at` s'y applique tel quel.
- **Zone de préparation.** `C:\ProgramData\Kura\staging` (SYSTEM et Administrateurs seuls),
  `/var/lib/kura/staging` (compte kura-agent, 0700) ; une préparation écrit dans `<id>.tmp`, renommé
  `<id>` une fois réussie ; chaque résultat est effacé après le dernier job qui l'envoie, et la
  zone est vidée au début et à la fin de chaque exécution, réussie ou non.
- **Échec.** Le job qui en dépend est sauté (code 2) : rien de partiel ni d'ancien n'est envoyé ;
  les autres jobs continuent. Une préparation ne tourne que si un job choisi en dépend (`run --job`),
  jamais en simulation. Résultat, nombre de fichiers et taille : `status.json`, `kura-agent status`,
  bilan (`prepares` dans le webhook), journal `prepare-<id>.log` de l'exécution.
- **Validation.** `${STAGING:x}` doit nommer une préparation, chaque préparation doit être envoyée par
  un job, `tools.zip` est obligatoire pour une préparation zip. `gpg` et `age` sont refusés par
  cette version.

### Export de bases (`db_export`)

L'agent lance l'outil d'export du moteur, une base après l'autre, et écrit un fichier par base.
L'export est cohérent sans arrêter la base : une seule transaction de lecture (PostgreSQL,
MySQL), ou la sauvegarde native de SQL Server. Recharger un export dans la base reste le
travail de l'administrateur (`pg_restore`, `mysql`, `RESTORE DATABASE`).

```yaml
tools:
  pg_dump: "C:/Program Files/PostgreSQL/17/bin/pg_dump.exe"   # /usr/lib/postgresql/16/bin/pg_dump
  mysqldump: "C:/Program Files/MariaDB 11.4/bin/mariadb-dump.exe"   # ou mysqldump
  sqlcmd: "C:/Program Files/Microsoft SQL Server/Client SDK/ODBC/170/Tools/Binn/SQLCMD.EXE"

prepare:
  - id: compta-db
    type: db_export
    engine: postgresql             # postgresql | mysql (MySQL, MariaDB) | sqlserver (Windows)
    host: localhost                # défaut ; port : 5432 ou 3306 par défaut
    user: kura_backup              # rôle en lecture seule
    password: credential:compta-db # jamais en clair ; demandé par install
    databases: [compta]            # absent : toutes, sauf les bases système
    timeout: 1h

  - id: gestion-db
    type: db_export
    engine: sqlserver
    instance: 'localhost\SQLEXPRESS' # instance locale seulement ; défaut localhost
    databases: [Gestion]           # sans user : authentification Windows du compte SYSTEM
    verify: true                   # RESTORE VERIFYONLY après chaque sauvegarde

jobs:
  - id: compta
    source: "${STAGING:compta-db}"
    destination: BASES/COMPTA
  - id: gestion
    source: "${STAGING:gestion-db}"
    destination: BASES/GESTION
```

|  | PostgreSQL | MySQL, MariaDB | SQL Server (Windows) |
| --- | --- | --- | --- |
| Outil (`tools`) | `pg_dump` 12 ou plus, au moins la version du serveur ; `psql`, `pg_restore`, `pg_dumpall` du même dossier | `mysqldump` ou `mariadb-dump` ; `mysql` ou `mariadb` du même dossier | `sqlcmd` (ODBC, ou go-sqlcmd avec TCP activé sur l'instance) |
| Commande | `pg_dump --format=custom` | `--single-transaction --routines --triggers --events --hex-blob --no-tablespaces` (+ `--set-gtid-purged=OFF --column-statistics=0` pour MySQL 8) | `BACKUP DATABASE … WITH COPY_ONLY, CHECKSUM, INIT`, plus `COMPRESSION` hors éditions Express et Web |
| Fichier | `<base>.dump`, et `globals.sql` (rôles, sans mots de passe) | `<base>.sql.gz` (compressé par l'agent) | `<base>.bak` |
| Mot de passe | `PGPASSWORD` du seul processus lancé | fichier d'options de la zone de préparation, effacé après | `SQLCMDPASSWORD` du seul processus lancé |
| Sans `user` | socket local (`socket: /run/postgresql`), authentification `peer` du compte `kura-agent` | socket local (`socket: /run/mysqld/mysqld.sock`), `unix_socket` | authentification Windows du compte SYSTEM |
| Contrôle | en-tête `PGDMP`, puis relecture complète par `pg_restore` | dernière ligne `-- Dump completed` | sommes de contrôle (`CHECKSUM`) ; `RESTORE VERIFYONLY` avec `verify: true` |

- **Rôle dédié, en lecture seule.**
  PostgreSQL : `CREATE ROLE kura_backup LOGIN PASSWORD '…'; GRANT pg_read_all_data TO kura_backup;`
  (PostgreSQL 14 ou plus). MySQL, MariaDB : `CREATE USER 'kura_backup'@'localhost' IDENTIFIED BY '…';
  GRANT SELECT, SHOW VIEW, TRIGGER, EVENT, LOCK TABLES, PROCESS ON *.* TO 'kura_backup'@'localhost';`.
  SQL Server : `CREATE LOGIN [NT AUTHORITY\SYSTEM] FROM WINDOWS;` puis, dans chaque base,
  `CREATE USER [NT AUTHORITY\SYSTEM]; ALTER ROLE db_backupoperator ADD MEMBER [NT AUTHORITY\SYSTEM];`
  (ou une connexion SQL dédiée, avec `user` et `password`).
- **Mot de passe** : `password: credential:<nom>` seulement ; `install` le demande s'il manque au
  coffre, `kura-agent credentials set <nom>` le change. Jamais en argument, jamais au journal.
- **Noms de fichiers** : le nom de la base ; un caractère qu'un système de fichiers refuserait
  est codé `%XX` (`L'Atelier` → `L%27Atelier.bak`). Deux bases qui ne différeraient que par la casse
  sont refusées. Le nom reste le même à chaque exécution : l'historique vient des versions du bucket.
- **SQL Server** écrit lui-même la sauvegarde : le temps de l'export, son compte de service
  (`NT SERVICE\MSSQL$<instance>`, et son compte de connexion s'il en a un propre) reçoit le droit
  d'écrire dans le seul dossier temporaire de la préparation ; ensuite l'agent reprend la propriété
  des fichiers (Administrateurs) et rétablit les droits de la zone. `COPY_ONLY` laisse intacte la
  chaîne de sauvegardes du client. Une base hors ligne (ou dans un autre état qu'`ONLINE`) est
  signalée en échec. Les commandes passent par `sqlcmd -b -x` : aucune substitution de variables,
  arrêt au premier échec ; noms de bases et chemins échappés.
- **PostgreSQL** : le nom de la base passe toujours entre apostrophes dans la chaîne de connexion
  (un nom contenant `=` ne peut pas désigner un autre serveur). `globals.sql` porte les rôles et
  tablespaces, à recréer avant de recharger un export sur un serveur neuf ; s'il ne peut pas être
  écrit, avertissement.
- **MySQL, MariaDB** : `--single-transaction` ne fige que les tables InnoDB ; les bases qui ont des
  tables MyISAM ou Aria sont signalées au journal. Utiliser l'outil du même éditeur que le serveur
  (`mariadb-dump` pour MariaDB).
- **Espace disque** : en plus de la réserve de 2 Go, l'espace libre doit dépasser de 20 % la taille
  du dernier export réussi (`prepare_last_size` dans `status.json`), sinon rien n'est lancé.
- **Échec** : une base en échec fait échouer la préparation (job sauté, aucun export ancien ou
  partiel envoyé) ; la sortie d'erreur de l'outil va au journal `prepare-<id>.log`. Les bases
  exportées s'affichent dans `kura-agent status` et `status.json` (`databases`).
- **`validate`** vérifie les outils et, pour une préparation avec `user`, se connecte et liste les
  bases qui seront exportées ; sans `user` (authentification du système), la connexion n'est
  vérifiée qu'à la première exécution, sous le compte de la tâche.

## Nouvelles versions : `kura-agent self-update`

```text
kura-agent self-update [--version vX.Y.Z] [--yes] [--config FICHIER]   installe (administrateur)
kura-agent self-update --rollback                                      remet la version précédente
kura-agent self-update --check [--version vX.Y.Z] [--json]             recherche seule, sans droits
```

`self-update` met à jour l'agent **installé** (`C:\Program Files\Kura`, `/usr/bin` pour le paquet
deb, sinon `/usr/local/bin`), pas forcément l'exécutable lancé : un agent plus récent téléchargé
à côté peut donc mettre à jour une installation ancienne. Déroulé :

1. droits d'administrateur (invite « Exécuter en tant qu'administrateur », `sudo`) ;
2. verrou `run.lock` : refus pendant une sauvegarde ; une sauvegarde planifiée qui tombe pendant
   la mise à jour attend sa fin (10 min au plus) au lieu d'être perdue ;
3. recherche et vérification de la release (ci-dessous), version visée 0.10.0 au moins et plus
   récente que la version installée ; espace libre d'au moins trois fois l'archive ;
4. confirmation (`--yes` hors d'un terminal) ;
5. téléchargement dans un dossier réservé aux administrateurs (`C:\Program Files\Kura\.update`,
   `/var/cache/kura-update`, `…/Kura/update` sous macOS), empreinte comparée à l'attestation,
   extraction des seuls exécutables, version annoncée par le nouvel exécutable contrôlée ;
6. remplacement : chaque exécutable renommé en `.old` (gardé pour `--rollback`), le nouveau mis à
   sa place ; Windows remplace ainsi `kura-agent.exe` et `kura-agentw.exe` même en cours
   d'exécution, puis relance les icônes ouvertes (lancé depuis `C:\Program Files\Kura` ;
   sinon, elles passent à la nouvelle version à la prochaine ouverture de session). Paquet
   deb : `dpkg -i` ;
7. finalisation par la nouvelle version (`self-update --finish`) : elle relit la configuration
   installée. Refus : retour automatique à l'ancienne version (code 6).

Configuration, secrets et planification ne sont pas touchés. L'état est dans `update.json` du
dossier de données (`kura-agent status` l'affiche) et le déroulé dans `update-<date>.log` du
dossier des journaux. Une coupure pendant le remplacement est reprise par la commande suivante
(`self-update`, ou `run` sous Windows et macOS), qui remet la version précédente.

Après chaque sauvegarde réelle, au plus une fois par 24 h, `run` cherche une version plus récente
(sans rien télécharger) et la note dans `update.json` ; `kura-agent status` l'annonce.

Une release n'est acceptée que si elle est publiée, ni brouillon ni préversion, **immuable**
(réglage « releases immuables » du dépôt), et si son **attestation de release** couvre
l'archive : GitHub signe pour chaque release immuable la liste des fichiers avec leur empreinte
SHA-256, le tag et le commit. L'agent la récupère par l'API des attestations et la vérifie avec
`sigstore-go`, comme `gh release verify` : certificat jusqu'à la racine de confiance de GitHub
embarquée (`internal/selfupdate/github_trusted_root.json`), horodatage signé par GitHub, identité
`https://dotcom.releases.github.com`, puis dépôt, tag, nom et empreinte de l'archive.

GitHub ajoute une autorité à sa racine environ tous les six mois. Le workflow
`trust-root.yml` compare chaque semaine la copie embarquée à celle du dépôt TUF de GitHub et
échoue quand elle est périmée : publier alors un agent avec la nouvelle racine. Un agent resté
sans mise à jour au-delà d'un changement d'autorité refuse les releases suivantes et se met à
jour une fois à la main.

Codes de sortie : 0 mis à jour, à jour (ou version de développement) · 3 version plus récente
disponible (`--check`) · 4 refusé (droits, sauvegarde en cours, version, confirmation, espace) ·
5 recherche, téléchargement ou vérification en échec (rien n'est modifié) · 6 nouvelle version en
échec, ancienne remise · 7 retour arrière impossible : réinstaller la version indiquée à la main.

Proxy : la tâche planifiée tourne sous le compte système, sans le proxy de l'utilisateur.
`update.proxy` dans la configuration (`http://proxy.example.com:3128`, sans identifiants) ;
sinon les variables `HTTPS_PROXY` / `NO_PROXY` de l'environnement. Seule la section `update`
est lue : une configuration invalide n'empêche pas la recherche.

## Restaurer : `kura-agent restore`

Le bucket garde, grâce au versioning, chaque version remplacée ou supprimée pendant 90 jours
(cycle de vie du modèle de bucket). `restore` lit ces versions avec la clé du poste
(`profile.aws_credentials`, droits `ListBucketVersions` et `GetObjectVersion`) et le SDK AWS pour
Go, sans passer par la CLI. Console d'administration (`sudo` sous Ubuntu et macOS). Rien n'est
jamais écrit dans le bucket : seules des lectures partent.

```powershell
# historique d'un fichier : date, taille, version actuelle, suppression
kura-agent restore "C:\Users\alice\Documents\Factures\2026.pdf" --versions

# ce qui serait restauré : un dossier tel qu'à la dernière sauvegarde, ou à une date
kura-agent restore "D:\Projets\Clients" --list
kura-agent restore "D:\Projets\Clients" --list --at 2026-10-20          # fin de journée
kura-agent restore "D:\Projets\Clients" --list --at "2026-10-20 18:00"  # heure locale

# restaurer dans un dossier à part
kura-agent restore "D:\Projets\Clients" --at 2026-10-20 --to "E:\Restauration"
kura-agent restore "C:\Users\alice\Documents\Factures\2026.pdf" --to "E:\Restauration"
kura-agent restore --all --at 2026-10-20 --to "E:\Restauration" --yes   # tous les jobs

# remettre en place les fichiers supprimés par erreur (ceux qui existent sont laissés)
kura-agent restore "D:\Projets\Clients" --at 2026-10-20 --in-place --list   # le plan, sans rien écrire
kura-agent restore "D:\Projets\Clients" --at 2026-10-20 --in-place
# revenir à l'état du 20 : remplacer aussi ce qui a changé, l'actuel gardé à côté
kura-agent restore "D:\Projets\Clients" --at 2026-10-20 --in-place --replace
```

Le chemin est rattaché au job dont la source le contient (la plus précise si des sources
s'imbriquent) ; sa clé dans le bucket est la destination du job suivie du chemin relatif. Sous
Windows la casse est ignorée, comme sur le disque. L'état à une date retient, fichier par fichier,
la dernière version enregistrée au plus tard à cette date ; un fichier supprimé à cette date n'y
figure pas. `--list` signale ce qui a changé depuis (« modifié depuis », « supprimé du poste le… »)
et les noms qu'une restauration refuserait (`..`, noms réservés de Windows, caractères interdits,
deux noms qui ne diffèrent que par la casse). Au-delà de 90 jours, l'état peut être incomplet : un
avertissement le rappelle. `--json` pour un traitement automatique.

**`--to DOSSIER`** : un dossier absent (créé) ou vide, hors des sources des jobs (la copie
partirait sinon à la sauvegarde suivante). Un dossier restauré y arrive sous son nom
(`E:\Restauration\Clients\…`), un fichier directement, et avec `--all` chaque job dans un
sous-dossier nommé d'après lui. Avant toute lecture : résumé (fichiers, volume, date de l'état,
destination, espace libre), espace libre comparé au volume avec 10 % de marge, puis confirmation
(`--yes` obligatoire hors d'un terminal). Ensuite :

- 8 fichiers lus à la fois ; chacun est écrit dans un fichier temporaire du dossier final, sa
  taille comparée à celle de la version (et son empreinte MD5 pour un envoi en une partie), puis
  renommé à son nom ; rien n'est jamais écrasé ;
- toutes les écritures passent par `os.Root` : rien ne sort de la destination, et un lien
  symbolique ou une jonction rencontré arrête le fichier concerné (signalé en échec) ;
- un fichier restauré porte la date de la sauvegarde qui l'a envoyé (la CLI AWS ne garde pas la
  date d'origine) ; le bucket ne garde pas non plus les droits : dans un dossier à part, les
  fichiers (de tous les comptes, avec `--all`) sont **réservés aux administrateurs** (Windows :
  SYSTEM et Administrateurs seuls ; Ubuntu et macOS : root, 0600, dossiers 0700), à remettre
  ensuite à leur propriétaire (`icacls`, `chown`) ou à restaurer en place ;
- interrompue (Ctrl+C, coupure) ou terminée avec des fichiers en échec, la restauration laisse
  `.kura-restore.json` et `.kura-restore.done` dans la destination : la même commande relancée
  saute ce qui est fait et ne relit que le reste ; une autre restauration vers ce dossier est
  refusée. Terminée sans échec, ces deux fichiers disparaissent.

**`--in-place`** : remet sous la source de leur job les fichiers absents (supprimés par erreur) ;
ceux qui existent sont laissés. Avec **`--replace`**, un fichier présent est comparé à la version
(taille, puis empreinte : MD5, ou MD5 des parties de 8 Mio pour un gros fichier envoyé en
plusieurs parties) ; s'il diffère, il est remplacé et l'actuel gardé à côté sous
`nom.avant-restauration-AAAAMMJJ-HHMM`. Un fichier ouvert par une application (Outlook et son
`.pst`) n'est pas remplacé : il est signalé en échec et rien ne change ; le restaurer avec
`--to`, Outlook fermé ensuite pour le remettre. La restauration en place prend le verrou de la
sauvegarde (`run.lock`) : refusée si une sauvegarde tourne, et la sauvegarde planifiée qui tombe
pendant la restauration se termine en « déjà en cours » (rattrapage selon `run_if_missed`).
`--list` avec `--in-place` montre le plan (remis, remplacés, laissés) sans prendre le verrou.
Relancée, la même commande ne refait que ce qui manque. Droits : un fichier **remplacé** reprend
ceux de l'actuel (Windows : son ACL, protection contre l'héritage comprise ; Ubuntu et macOS :
mode, propriétaire et groupe). Un fichier **remis** (absent) prend, sous Windows, les droits du
dossier qui le reçoit ; sous Ubuntu et macOS, le propriétaire et le groupe de ce dossier, en
0600 (dossiers créés 0700) : jamais lisible par tous. Un fichier système qui demandait d'autres
droits (`/etc/passwd`, contenu d'un site web…) est à corriger après coup. Les préfixes en
quarantaine sont refusés, sauf `--include-quarantined`.

Journal : `restore-AAAAMMJJ-HHMMSS.log` dans le dossier des journaux, une ligne par fichier
(`OK`, `REMPLACÉ`, `PRÉSENT`, `IDENTIQUE`, `DÉJÀ`, `REFUSÉ`, `ÉCHEC`, avec version, date et
taille), la commande et le compte.
`kura-agent status` affiche la dernière restauration. Chaque restauration est signalée au bilan
(voir le webhook ci-dessus) ; `--versions` et `--list` ne le sont pas.

Chaque compte de l'icône peut aussi restaurer ses propres fichiers depuis l'icône, sans droits
d'administrateur (voir « L'icône »).

Codes de sortie : 0 restauration faite, ou rien à restaurer · 1 restauration partielle ou
interrompue (relancer la même commande) · 3 configuration illisible, chemin hors des sources des
jobs, clé absente · 4 refusé (droits, options, destination, confirmation, espace disque) ·
5 bucket injoignable ou clé refusée, rien n'a été écrit.

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
├── restore.json             dernière restauration (kura-agent status)
├── credentials\             secrets chiffrés (administrateurs seulement)
├── staging\                 préparations (archives) le temps d'une exécution, vidée à la fin
├── pending\                 bilans webhook en attente de renvoi
└── logs\
    ├── restore-20261028-111203.log   une restauration, une ligne par fichier
    └── runs\2026-10-24_2230\
        ├── run.log          déroulé de l'exécution
        ├── data.log         commandes et sortie brute d'aws pour le job "data"
        ├── prepare-courrier.log   sortie de l'outil zip de la préparation "courrier"
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
internal/lock/      verrou d'exécution (run.lock)
internal/state/     status.json, history.jsonl
internal/report/    bilan vers healthchecks ou webhook
internal/ipc/       dialogue run ↔ icône : messages JSON, canal nommé par session
internal/tray/      icône : modèle d'affichage (portable), interface Win32
internal/creds/     coffre de secrets (DPAPI sous Windows)
internal/platform/  fonctions dépendantes du système : ce qui est écrit pour chacun, « non pris en charge »
internal/systemd/   planification Linux : kura-sync.service et kura-sync.timer, systemctl
internal/setup/     ACL, copie des fichiers, icône au démarrage, ancien compte svc-kura (install)
internal/awsinstall/ CLI AWS absente : MSI fourni ou téléchargé, signature Amazon vérifiée, msiexec
internal/schedtask/ tâches \Kura\Sync et \Kura\Restore : XML du Planificateur, schtasks
internal/paths/     emplacements sur disque
internal/selfupdate/ nouvelles versions : releases GitHub, attestation de release vérifiée (sigstore-go)
internal/prepare/   préparations : outils vérifiés (droits, version), zone de préparation, archive zip
internal/restore/   restauration : chemin → clé, versions du bucket (SDK AWS), état à une date, écriture confinée
```

`internal/runner` ne dépend d'aucune interface graphique : ses tests utilisent une fausse CLI AWS
et un faux gestionnaire de processus (nuit normale, garde-fou, `--force-deletes`, fichiers verrouillés,
erreurs passagères et définitives, Outlook impossible à fermer, source absente, simulation, annulation).
