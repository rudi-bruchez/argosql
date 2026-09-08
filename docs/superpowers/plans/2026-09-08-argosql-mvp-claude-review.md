# Review externe du plan — Claude Code

Date : 2026-09-08. Processus neuf, mode safe, outil Read uniquement, sans conversation ni reviews antérieures. Les deux fichiers ont été copiés dans un répertoire temporaire isolé.

Prompt exact :

> Relis plan.md et spec.md. Que penses-tu du plan d’implémentation ?

Modèle rapporté : claude-haiku-4-5-20251001, claude-opus-5[1m]. Durée rapportée : 172526 ms. Résultat : success.

Empreintes SHA-256 des documents relus (avant les corrections de suivi) :

- `plan.md` : `db6bd7e2529b7fca2869345c520e4868654b12b2100ffb0542127361d139590b`
- `spec.md` : `1f888c9537ba900a112c2b5dbf52751b94da87275a2d5dd4ba5443b3f423a534`

Les numéros de ligne ci-dessous visent ces copies. Réponse reproduite sans modification ; elle ne constitue pas une exécution des tests proposés.

---

J'ai relu les deux documents. Verdict court : **le plan est solide et nettement au-dessus de la moyenne** — traçabilité spec→tâches complète, cycles rouge/vert avec assertions concrètes, tests de frontière exacte (N vs N+1), permissions tri-état, honnêteté sur la couverture Windows/2025. Mais il reste **six points qui bloqueront ou coûteront cher à l'exécution**, dont deux vrais défauts de conception d'interface.

## Défauts de conception à corriger avant de lancer la tâche 5

**1. Incompatibilité push/pull entre `output.WriteTable` et `model.Sink`**
`plan.md:276` définit `WriteTable(w, format, spec, next func() ([]Cell, error))` — modèle *pull*. `plan.md:95-101` définit `Sink` avec `Begin/Row/End` — modèle *push*, et `plan.md:304` dit que le collecteur « consomme model.Sink et output.WriteTable ». Implémenter `Collector.Row()` au-dessus d'un writer qui tire les lignes impose une goroutine + channel par table, avec la propagation d'erreur et l'annulation qui vont avec. Remplacer par un encodeur incrémental : `NewTableEncoder(w io.Writer, format string, spec TableSpec) (*Encoder, error)`, `WriteRow([]Cell) error`, `Close() error`. La forme *pull* peut rester un wrapper de confort pour les tests de la tâche 5.

**2. L'aperçu par relecture des artefacts (tâche 7) crée un composant non planifié**
`plan.md:344` : « Il construit les candidats d'aperçu lors d'une seconde lecture des artefacts ». Relire un TSV échappé ou un JSON positionnel exige un **décodeur** qui n'existe nulle part dans la carte des fichiers ni dans la tâche 5. La motivation (ne pas garder 100 MiB en RAM avec `--no-truncate`) est légitime mais se résout plus simplement : retenir les N premières lignes en mémoire pendant `Row()`, en tronquant chaque cellule candidate à `ByteLimit+ε` (32 KiB) — aucune cellule au-delà ne peut jamais être affichée. Borne mémoire ~ N × colonnes × 32 KiB, sans parseur inverse.

## Trous d'attribution (rien ne les implémente)

**3. Valeurs par défaut de chemins et jeu de flags globaux.** `Load(path, name, ...)` (tâche 2) reçoit un chemin, mais *personne* ne calcule le défaut `os.UserConfigDir()/argosql/config.yaml` (spec:115). Idem `--out-dir` et le défaut « répertoire cache OS » (spec:109) : `artifacts.New(dir, ...)` reçoit un `dir` sans propriétaire. Plus généralement, aucune tâche n'énumère les flags globaux (`--ctx --db --config --format --timeout --preview --truncate --no-truncate --out-dir`) ni leur table de validation ; la tâche 9 se contente de « Request contient champs typés des options du plan ». Conséquence concrète : le conflit `--no-truncate` + `--truncate` explicite → code 2 (spec:97) n'est couvert que par le générique « doublons contradictoires ».

**4. Règle de sélection SQL par version non définie.** Les tâches 10-11 créent `top_2019.sql`/`top_2022.sql` et `Session.Major`, mais aucune règle ne dit ce que fait Major=17 (2025), alors que le smoke 2025 de la tâche 15 exécute `top/query/plan`. Il faut écrire explicitement « >= 16 utilise la variante 2022 » et le tester.

**5. Type SQL des colonnes calculées.** La règle « bigint/decimal en chaînes JSON » (spec:107) est pilotée par `Column.SQLType`, mais aucune tâche ne fixe le type déclaré de `cpu_total_ms`, `cpu_avg_ms`, `reads_total`… Ce n'est pas cosmétique : si l'agrégat SQL est un `float`, la fixture de la tâche 10 qui attend exactement `cpu_avg_ms=2.8` compare un flottant non représentable. Choisir `decimal(38,x)` côté SQL (ou une tolérance explicite côté test) et déclarer le SQLType dans la `TableSpec`.

**6. `Lab.Run` a deux définitions contradictoires.** Tâche 11 (`plan.md:530`) : « appelle cli.Run et décode model.Result » — en process. Tâche 15 (`plan.md:673`) : « Les tests CLI par sous-processus construisent le binaire une fois par suite ». Deux problèmes : (a) l'appel en process viole la règle « Aucune couche ne dépend de `internal/cli` en dehors de cmd/asq » (`plan.md:54`) ; (b) le code 130 sur SIGINT et la fermeture de stdout ne sont pas testables en process. Trancher pour le sous-processus dès la tâche 11, avec le binaire construit une fois.

## Points à préciser

- **Manifeste (tâche 6, `plan.md:326`)** : « retirer des données non finalisées avant la fin » doit dire explicitement *temporaires non finalisés uniquement* — la spec (spec:111) impose de conserver les artefacts déjà complets.
- **Propriété de `rows_shown` / `preview_complete` / `omitted_reasons`** : remplis par le collecteur ou par `Render` ? `Render` a une signature pure (`plan.md:342`) alors que ces champs ne sont connus qu'après rendu. À arbitrer en une ligne, sinon la tâche 7 modifiera les structures de la tâche 1 « sans changer les signatures ».
- **`go.yaml.in/yaml/v4`** : prévoir explicitement le repli sur `v3` si v4 n'est pas publié, et vérifier que la variante retenue rejette bien les clés dupliquées *et* les champs inconnus (`KnownFields`).
- **Fixture Query Store** : la tâche 4 dit « créer AppDB avec Query Store » sans paramètres. Sans `INTERVAL_LENGTH_MINUTES = 1`, `DATA_FLUSH_INTERVAL_SECONDS` court et `sp_query_store_flush_db`, les tests de la tâche 10 seront non déterministes ou lents.
- **Formulation tâche 10** : « autre replica_group, exécutions 3/4 à exclure » est ambigu — sur 2022 c'est une **ligne distincte** du classement (spec:79), pas une exclusion.

## Dimensionnement

Les tâches **4** et **9** concentrent le risque. La 4 empile harness Podman + génération CA/certs x509 + montage `mssql.conf` avec contraintes d'UID rootless + 3 scénarios TLS d'échec + conflit de verrou réel + interruption. La 9 empile registre + parseur + `main` + signaux + `info` + `health` + `status` + décodage bitmask + test d'intégration. Je les couperais en 4a (harness + preuve session) / 4b (matrice TLS) et 9a (CLI + help offline) / 9b (info + qs status). Cela ne change ni la spec ni la traçabilité.

Sur la CI (tâche 16) : deux images SQL Server dans un même job avec `-timeout=15m` est optimiste (démarrage moteur + attentes de flush). Jobs séparés par image, timeout plus large.

## Détails mineurs

- `testdriver_test.go` (tâche 3) et `faults_test.go` (tâche 15) contredisent la convention « même nom que le fichier testé » (`plan.md:54`) — ce sont des helpers, autoriser l'exception explicitement.
- Le « rouge » de la tâche 4 est un échec de compilation (`NewLab` absent), ce que le plan lui-même disqualifie comme rouge fonctionnel (`plan.md:32`). Assumer la nuance : rouge de compilation acceptable pour l'introduction d'un symbole, interdit pour un échec d'environnement.

## Ce que je garderais tel quel

La table de traçabilité, la séparation stricte collecte/aperçu, les tests de frontière exacte (N vs N+1, octets avant/à/après cap), le `permissionFromSQL(NULL) == Unknown`, l'ordre résolution→permission (8 avant 4), l'instrumentation anti-DOM de la tâche 12, l'hygiène de commit par tâche avec interdiction de `git add .`, et le refus de revendiquer une couverture Windows non obtenue.

Si tu veux, je peux appliquer directement ces corrections à `plan.md` : les six premiers points sont des modifications localisées (signatures des tâches 5/6/7, une tâche 9 étendue pour les défauts de chemins/flags, règle de version, `Lab.Run` unifié) et je peux les faire sans toucher au découpage en tranches, ou avec le découpage 4a/4b et 9a/9b si tu le veux aussi.
