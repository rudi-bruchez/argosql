# argosql MVP Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Livrer `asq`, un CLI Go de diagnostic SQL Server dont les résultats et artefacts sont exploitables par un agent sans saturer son contexte.

**Architecture:** Une connexion SQL conservée pendant chaque invocation exécute des requêtes embarquées et paramétrées. Les diagnostics produisent des tables typées vers un collecteur qui écrit les exports complets, puis le moteur de sortie construit un aperçu borné. La configuration, la session, les diagnostics, le parseur XML et la sérialisation ont des frontières testables séparément.

**Tech Stack:** Go 1.27, `database/sql`, `github.com/microsoft/go-mssqldb v1.11.0`, YAML, bibliothèque standard pour CLI/JSON/XML/tests, Podman pour SQL Server 2019/2022 et smoke test 2025. Utiliser le module YAML maintenu `go.yaml.in/yaml/v4`, résoudre sa version à la tâche 2 et la figer dans go.mod/go.sum. Mesuré au 8 septembre 2026 : la seule version publiée est `v4.0.0-rc.6`. La décision est donc de figer une pré-version, pas de choisir entre deux versions stables ; l'écrire ainsi dans docs/testing.md à la tâche 16, avec la date de la mesure. Sa strictesse a été vérifiée : champs inconnus, clés dupliquées et profils dupliqués sont bien rejetés. [Source YAML](https://github.com/yaml/go-yaml), [pilote SQL](https://github.com/microsoft/go-mssqldb/tree/v1.11.0).

**Spec:** [2026-09-08-argosql-mvp-design.md](../specs/2026-09-08-argosql-mvp-design.md). La spec corrigée prime sur le rapport de review historique. La demande de plan autorise sa rédaction ; ce document n'annonce ni code implémenté ni tests déjà passés.

## Global Constraints

- SQL Server 2019 et 2022 ; clients Linux amd64 et Windows amd64. SQL authentication ; SQL Server 2025 reçoit un smoke test seulement.
- Binaire `asq` ; module `github.com/rudi-bruchez/argosql` correspondant au remote du dépôt.
- `encrypt=true`, `TrustServerCertificate=true` par défaut, y compris champ YAML absent. `trust_server_certificate: false` active la validation. Aucun repli TLS ni confirmation du défaut.
- Une seule `*sql.Conn` conservée ; `SET LOCK_TIMEOUT 5000` puis vérification ; timeout global 30 s par défaut, 1–300 s ; connexion 5 s au maximum, dans le budget global.
- Aperçu : 10 lignes/table, 200 points de code/cellule ; stdout complet <= 32,768 octets, newline comprise. `--preview` 0–10,000 ; `--truncate` 1–10,000 ; `--no-truncate` ne désactive pas le plafond d'octets.
- Collecte <= 10,000 lignes et 104,857,600 octets d'artefacts, manifeste compris. Le dépassement de collecte donne 7 ; une simple réduction d'aperçu conserve 0.
- UTF-8 sans BOM ; TSV échappé et JSON à tableaux de lignes positionnels ; bigint/decimal JSON en chaînes avec métadonnées de type. Aucun arrondi dans les exports complets.
- Codes : 0 succès, 2 arguments/config, 3 connexion/auth/TLS, 4 permission/fonction indisponible, 5 exécution/timeout, 6 fichier/sérialisation, 7 plafond de collecte, 8 absent ou invisible, 130 interruption.
- Les droits sont des capacités effectives, pas un booléen « connexion sûre ». Garder `not_found_or_not_visible` lorsque la distinction ne peut pas être prouvée.
- Requêtes intégrées uniquement ; pas de commande SQL arbitraire, MCP, maintenance, script de correction ou auth intégrée dans ce plan.
- Aucun conteneur/volume existant ne sert de fixture. Utiliser des conteneurs dédiés et des ports loopback attribués par Podman. Ne nettoyer que les ressources du run.
- Les changements documentaires présents et `docs/TASKS01.md` non suivi appartiennent au travail existant. Ne pas les écraser ni les inclure accidentellement dans un commit de code.

## Organisation et méthode d'exécution

Exécuter les tâches dans l'ordre. Chaque tâche a un cycle rouge/vert et une sortie vérifiable ; ne pas publier ni déployer à la fin. À l'exécution, utiliser le skill de worktree pour isoler le code si nécessaire et y transporter explicitement la spec/ce plan, y compris leurs modifications non committées. Ne pas créer maintenant de worktree pour cette seule rédaction.

Les blocs SQL de ce plan sont des esquisses du noyau, jamais le contenu final d'un fichier `.sql`. La prose qui suit chaque bloc est normative et l'étend : colonnes supplémentaires, variante par version du moteur, jointures de noms. Un implémenteur qui colle le bloc dans le fichier et écrit ses tests à partir du même bloc obtient une suite verte qui a perdu l'exigence. Chaque tâche portant un bloc SQL nomme ci-dessous l'assertion qui prouve que l'extension est présente. Un panel de relecture a produit trois faux positifs sur ce seul motif : la lecture verbatim du bloc est la lecture naturelle, c'est pourquoi elle est interdite ici explicitement.

Les blocs de tests sont des cas directeurs à placer dans les fichiers indiqués, dans le package concerné, avec les imports standard nécessaires. Les listes de cas adjacentes sont également obligatoires. Les blocs d'algorithme ne dispensent pas des contrats de la spec. Chaque commande `go test -run` de ce plan est suivie du nombre de tests que le filtre doit sélectionner, sous la forme « attendu : N tests ». Mesuré : `go test -run 'TestUnknown|TestResolve'` affiche `ok` et sort avec le code 0 quand `TestResolve` n'existe pas, sans le moindre avertissement. Un nombre inférieur à N veut dire que le filtre est faux ou que le test n'a pas été écrit, jamais que le travail est fait. Vérifier le compte avec `go test -run <filtre> -v | grep -c '^=== RUN'` avant de conclure au vert. Les filtres de ce plan nomment plusieurs tests que les tâches ne définissent pas ; les corriger en écrivant la tâche plutôt qu'en élargissant le filtre.

Les commandes de test doivent d'abord échouer pour la raison attendue, puis réussir après implémentation ; une erreur de compilation sur un nouveau symbole est un rouge d'introduction acceptable ; un échec d'installation ou d'environnement n'est pas un rouge fonctionnel.

Après chaque tâche validée : inspecter le diff, ajouter uniquement les fichiers de cette tâche et créer un commit avec le message proposé. Commandes : `git add -- <chemins de la tâche>` puis `git commit -m '<message proposé>'`. Ne jamais utiliser `git add .` pour absorber les fichiers de l'utilisateur. Un point de revue suit chaque tranche.

### Carte des fichiers

| Emplacement | Responsabilité |
| --- | --- |
| `cmd/asq/main.go` | Signaux, flux standard, code de sortie |
| `internal/cli/{parse,registry,run}.go` | Arguments, découverte, orchestration |
| `internal/model/{result,error}.go` | Types échangés et erreurs publiques |
| `internal/config/{load,tls}.go` | YAML strict, secrets, TLS explicite |
| `internal/sqlserver/{session,permissions,objects}.go` | Connexion tenue, capacités, résolution |
| `internal/diagnostics/*.go`, `internal/diagnostics/sql/*.sql` | SQL embarqué et contrats des commandes |
| `internal/output/{cell,scan,tsv,json,decode,preview}.go` | Conversion des valeurs du pilote, formats et budget stdout |
| `internal/artifacts/{store,collector,manifest}.go` | Flux d'export, quotas et complétude |
| `internal/plan/{xml,summary}.go` | Export XML et résumé incrémental |
| `tests/integration/{podman,fixture,tls}_test.go` | Fixtures réelles isolées |
| `tests/integration/sql/*.sql` | Schéma, charge et permissions rejouables |
| `.github/workflows/ci.yml`, `Makefile` | Tests locaux/CI et compilation |
| `docs/usage.md`, `docs/permissions.md`, `docs/testing.md`, `skills/argosql/SKILL.md` | Utilisation et limites vérifiées |

Tous les fichiers listés sont à créer sauf README.md et .gitignore. Les tests unitaires portent le même nom que le fichier testé avec `_test.go` ; les helpers partagés peuvent avoir un nom propre comme testdriver_test.go. Aucun package de production ne dépend de internal/cli en dehors de cmd/asq. Les tests externes d'intégration utilisent le binaire en sous-processus.

### Interfaces communes à fixer dans la tâche 1

```go
// internal/model : champs exportés sérialisés en snake_case.
type Cell any // nil, string, bool, int64, float64 ; SQL exact en string si nécessaire
// Type nu et non struct à un champ : une struct exige un MarshalJSON pour se
// sérialiser en valeur nue plutôt qu'en {"Value":"abc"}, un type nu n'exige
// rien. Mesuré identique octet pour octet. Les consommateurs font leur
// type-switch sur la Cell elle-même, pas sur un champ de la Cell.
// Vocabulaire fermé des raisons de réduction. Toute autre valeur est un défaut.
const (
    ReasonPreviewOmitted     = "preview_omitted"     // cellule trop grosse pour le budget
    ReasonRowsTruncated      = "rows_truncated"      // lignes retirées pour tenir
    ReasonCellTruncated      = "cell_truncated"      // troncature par runes
    ReasonEncodingNormalized = "encoding_normalized" // remplacement de séquence invalide
)
// Enveloppe de dernier recours quand le résultat lui-même ne tient pas dans le
// budget. Type dédié : model.Result ne peut pas produire ce littéral, et
// omitempty ferait disparaître "ok":false que la spec exige.
type FallbackError struct {
    SchemaVersion int          `json:"schema_version"`
    OK            bool         `json:"ok"`
    Error         *PublicError `json:"error"`
}
type Column struct { Name, SQLType string }
type TableSpec struct { Name string; Columns []Column }
type Notice struct { Kind, Message, Table string }
// Completeness ne décrit que la collecte. Le collecteur la remplit et c'est
// elle, et elle seule, que le manifeste sérialise.
type Completeness struct {
    RowsCollected int64
    CollectionComplete, PropertiesComplete bool
}
// PreviewState ne décrit que l'aperçu. Render la remplit sur sa copie du
// résultat. Le collecteur ne l'écrit jamais et le manifeste ne la contient
// jamais : un manifeste qui porterait rows_shown=0 promettrait faussement
// qu'aucune ligne n'a été affichée.
type PreviewState struct {
    RowsShown int64
    PreviewComplete bool
    OmittedReasons []string
}
type TableResult struct {
    Spec TableSpec
    Rows [][]Cell // aperçu seulement, jamais la collecte entière
    State Completeness
    Preview PreviewState
}
type Artifact struct { Kind, Path string; Bytes int64; Complete bool }
type ContextInfo struct {
    Server, Database, Principal, Version string
    TLSEncryption, CertificateValidation string
    CollectedAt time.Time
}
type PublicError struct { Code int; Kind, Message string; SQLNumber int32 }
func (e *PublicError) Error() string
func ExitCode(err error) int // nil->0, PublicError.Code, sinon 5

type Result struct {
    SchemaVersion int
    OK bool
    Context ContextInfo
    Tables []TableResult
    Notices []Notice
    Artifacts []Artifact
    ManifestPath string
    Error *PublicError
}
// Chaque diagnostic ferme ses rows SQL ; Sink n'exécute pas de SQL.
type Sink interface {
    Begin(TableSpec) error
    Row([]Cell) error
    End(collectionComplete, propertiesComplete bool) error
    File(kind, suffix string, src io.Reader) (Artifact, error)
    Notice(Notice)
}
```

Les interfaces ci-dessus définissent les signatures, pas une obligation de mélanger logique et DTO. Les détails privés (writer courant, réservation d'octets, erreur interne originale) restent dans leurs packages. Le contrat SQL reste `*sql.Conn` : les faux de pilote passent par `database/sql/driver`, sans simuler SQL Server avec une base différente.

## Tranche 1 — socle et première commande réelle

### Tâche 1 : erreurs et contrat de résultat

**Fichiers :** créer `go.mod`, `internal/model/result.go`, `internal/model/error.go`, `internal/model/error_test.go`.

**Interfaces :** produit les types communs ci-dessus ; aucune dépendance interne.

- [ ] Initialiser le module : `go mod init github.com/rudi-bruchez/argosql`. Fixer `go 1.27.0` ; aucune dépendance téléchargée pour cette tâche.
- [ ] Écrire le test :

```go
func TestExitCode(t *testing.T) {
    for _, code := range []int{2,3,4,5,6,7,8,130} {
        err := fmt.Errorf("context: %w", &PublicError{Code: code, Kind: "fixture"})
        if got := ExitCode(err); got != code { t.Fatalf("got %d want %d", got, code) }
    }
    if ExitCode(nil) != 0 { t.Fatal("nil must succeed") }
}
```

- [ ] Exécuter `go test ./internal/model -run TestExitCode -v` ; attendu : 1 test ; rouge sur les symboles manquants.
- [ ] Implémenter les types ; `ExitCode` utilise `errors.As` pour préserver le code à travers les wrappers :

```go
func ExitCode(err error) int {
    if err == nil { return 0 }
    var public *PublicError
    if errors.As(err, &public) { return public.Code }
    return 5
}
```

- [ ] Vérifier le même test et la sérialisation snake_case par un test JSON avec `schema_version`, `rows_collected` et `certificate_validation`.
- [ ] Commit : `feat: define diagnostic result and error contracts`.

### Tâche 2 : profils YAML et options TLS

**Fichiers :** créer `internal/config/load.go`, `internal/config/tls.go`, `internal/config/load_test.go`, `internal/config/tls_test.go` ; modifier go.mod, créer go.sum.

**Interfaces :** consomme model.PublicError ; produit :

```go
type Profile struct {
    Host, Database, Username, Password, CAFile string
    Port int
    TrustServerCertificate bool
}
func Load(path, name, databaseOverride string, getenv func(string) string) (Profile, error)
func DSN(p Profile) (string, error) // interne ; jamais afficher son résultat
```

- [ ] Installer le pilote fixé et résoudre une seule fois YAML : `go get github.com/microsoft/go-mssqldb@v1.11.0 go.yaml.in/yaml/v4`. Noter la version YAML résolue dans docs/testing.md à la tâche 16 ; go.mod/go.sum la figent immédiatement.
- [ ] Écrire le test de défaut sur une config temporaire :

```go
func TestDefaultTrust(t *testing.T) {
    path := filepath.Join(t.TempDir(), "config.yaml")
    text := "profiles:\n  lab:\n    host: localhost\n    database: AppDB\n    username: reader\n    password_env: DB_PASSWORD\n"
    if err := os.WriteFile(path, []byte(text), 0600); err != nil { t.Fatal(err) }
    p, err := Load(path, "lab", "", func(string) string { return "secret" })
    if err != nil { t.Fatal(err) }
    if !p.TrustServerCertificate || p.Port != 1433 { t.Fatalf("bad defaults") }
    dsn, err := DSN(p); if err != nil { t.Fatal(err) }
    u, err := url.Parse(dsn); if err != nil { t.Fatal(err) }
    if u.Query().Get("encrypt") != "true" || u.Query().Get("TrustServerCertificate") != "true" { t.Fatal("TLS defaults") }
}
```

- [ ] Rouge : `go test ./internal/config -v`.
- [ ] Décoder via un type brut utilisant `*bool` pour distinguer absent/false. Rejeter champs inconnus, doublons, YAML supplémentaire, username/password_env manquants, port hors 1–65535, base finale vide ; résolution relative ca_file, parsing PEM/DER. Mesuré : `go-mssqldb` v1.11.0 dispatche sur l'extension du fichier dans `msdsn.Parse` et n'accepte que `.pem` et `.der` ; un PEM parfaitement valide nommé `ca.crt`, `ca.cer` ou sans extension rend `certificate type .crt is not supported` au moment de la connexion, donc un code 3, alors que la spec exige un code 2 avant connexion. Valider ca_file dans Load, avant toute construction de DSN : rejeter avec 2 toute extension autre que `.pem` ou `.der`, en nommant les deux extensions acceptées dans le message. Lire et parser le contenu soi-même, et vérifier la valeur de retour de `AppendCertsFromPEM` : le pilote l'ignore, si bien qu'un `.pem` au contenu invalide produit un pool vide et un échec de chaîne à la connexion au lieu d'une erreur de configuration. Un `.pem` dont le contenu ne fournit aucun certificat vaut 2. Utiliser `url.UserPassword`, `net.JoinHostPort`, `url.Values`, jamais interpolation de credentials :

```go
q := url.Values{"encrypt": {"true"}, "TrustServerCertificate": {strconv.FormatBool(p.TrustServerCertificate)}, "database": {p.Database}}
if p.CAFile != "" { q.Set("certificate", p.CAFile) }
u := url.URL{Scheme:"sqlserver", Host:net.JoinHostPort(p.Host, strconv.Itoa(p.Port)), User:url.UserPassword(p.Username,p.Password), RawQuery:q.Encode()}
```

- [ ] Vert : tests true/false/absent, ca_file+true rejeté, override base, caractères `@;?` du secret, champs sensibles interdits et erreurs YAML reformulées sans afficher la source. Aucun TLS flag global ajouté. Cas explicites du ca_file : même contenu PEM valide nommé `ca.pem` accepté, `ca.crt` rejeté avec 2, sans extension rejeté avec 2, `.pem` au contenu vide ou corrompu rejeté avec 2. La matrice TLS de la tâche 4b génère ses propres certificats et ne rencontrera jamais ces cas : ils appartiennent aux tests unitaires de cette tâche.
- [ ] Commit : `feat: load strict connection profiles with trusted TLS default`.

### Tâche 3 : session tenue, délais et nettoyage

**Fichiers :** créer `internal/sqlserver/session.go`, `internal/sqlserver/session_test.go`, `internal/sqlserver/testdriver_test.go`.

**Interfaces :** consomme config.Profile ; produit :

```go
type Session struct { Conn *sql.Conn; Major int; /* pool privé */ }
func Open(ctx context.Context, p config.Profile) (*Session, error)
func (s *Session) Close() error
```

L'appelant crée le contexte global avant Open. Major est obtenu par SERVERPROPERTY après setup. Major=15 utilise la variante SQL 2019 ; Major=16 ou 17 utilise la variante 2022. Major=17 est accepté comme 16, et l'avis de compatibilité non validée qui l'accompagne n'appartient pas à cette tâche : `Session` n'a pas d'accès au `Sink`, c'est `internal/cli.Run` qui l'émet à la tâche 9 après `Open`. Toute autre version retourne 4 avant les requêtes spécifiques. Tester 15/16/17 et une version inconnue ; ne pas sélectionner automatiquement une variante pour toutes les versions futures.

- [ ] Test du faux pilote : enregistrer un pilote minimal implémentant Conn, QueryerContext, ExecerContext et SessionResetter. Enregistrer les événements et remettre lockTimeout=-1 dans ResetSession. Le test doit constater 5000 au deuxième SELECT via Session.Conn.

Mesuré avec un faux pilote sur Go 1.27 : `ResetSession` n'est pas appelé quand la connexion revient au pool. Après `Conn.Close()` le journal du pilote est inchangé et le témoin vaut encore 5000. Le reset ne se déclenche qu'à l'acquisition suivante, avant la première requête de la reprise. Le témoin doit donc être lu par un `db.Conn` ultérieur, pas après `Close`. Écrire l'assertion sous cette forme : après `Close`, aucun événement `ResetSession` dans le journal ; après un nouveau `db.Conn` suivi d'un SELECT, l'événement est présent et la valeur est -1. Une étape qui prétend observer -1 juste après la remise au pool observe 5000 et sera « corrigée » jusqu'à passer, sans que personne ne comprenne pourquoi.

```go
func TestHeldConnection(t *testing.T) {
    // openRecordedSession est défini dans testdriver_test.go : ouvre Session
    // via le constructeur privé commun à Open et renvoie le journal du pilote.
    s, events := openRecordedSession(t)
    defer s.Close()
    var value int
    if err := s.Conn.QueryRowContext(context.Background(), "SELECT @@LOCK_TIMEOUT").Scan(&value); err != nil { t.Fatal(err) }
    if value != 5000 { t.Fatal(value) }
    for _, event := range *events { if event == "reset-after-setup" { t.Fatal(event) } }
}
```

- [ ] Rouge : `go test ./internal/sqlserver -run TestHeldConnection -v` ; attendu : 1 test.
- [ ] Construire le pool, acquérir Conn avec un contexte enfant <=5 s, fermer ce contexte après acquisition, puis utiliser le contexte global pour setup et diagnostics :

```go
connectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
conn, err := db.Conn(connectCtx)
cancel()
if err != nil { db.Close(); return nil, &model.PublicError{Code:3, Kind:"connection", Message:"connection failed"} }
// Garder conn ; SET et SELECT de contrôle utilisent ctx, pas connectCtx.
```

Exécuter `SET LOCK_TIMEOUT 5000`, lire la valeur, détecter mismatch ; tout échec ferme Conn puis DB. Fermer les Rows sur toutes les branches. Mapper erreur SQL 229/300 vers permission uniquement après connexion ; conserver SQLNumber sans DSN/secret. N'ajouter aucune boucle de retry.
- [ ] Vert : faux délais connexion/exécution, contexte déjà annulé, erreur setup, Close idempotent ; `go test ./internal/sqlserver -v`.
- [ ] Commit : `feat: hold SQL session settings across diagnostics`.

### Tâche 4 : harness Podman et preuve session/TLS

**Deux unités de revue successives :** 4a termine le harness/readiness/cleanup et les assertions session/verrou ; 4b ajoute génération de certificats et matrice TLS. Faire un cycle rouge/vert et commit séparé pour chaque unité.

**Fichiers :** créer `tests/integration/podman_test.go`, `tests/integration/fixture_test.go`, `tests/integration/tls_test.go`, `tests/integration/session_test.go`, `tests/integration/sql/bootstrap.sql`, `docs/testing.md`.

**Interfaces :** helpers de test locaux :

```go
type Lab struct { Admin *sql.DB; Profile config.Profile; ContainerID, ImageID string }
func NewLab(t *testing.T, image string) *Lab
func NewTLSLab(t *testing.T, mode string) *Lab // valid, wrong-host, expired
```

- [ ] Écrire ce test avant le harness :

```go
func TestSessionTLS(t *testing.T) {
    lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second); defer cancel()
    s, err := sqlserver.Open(ctx, lab.Profile); if err != nil { t.Fatal(err) }; defer s.Close()
    var lock, spid int
    if err := s.Conn.QueryRowContext(ctx,"SELECT @@LOCK_TIMEOUT, @@SPID").Scan(&lock,&spid); err != nil { t.Fatal(err) }
    var encrypted string
    if err := lab.Admin.QueryRowContext(ctx,"SELECT encrypt_option FROM sys.dm_exec_connections WHERE session_id=@p1",spid).Scan(&encrypted); err != nil { t.Fatal(err) }
    if lock != 5000 || encrypted != "TRUE" { t.Fatalf("lock=%d encrypted=%s",lock,encrypted) }
}
```

- [ ] Rouge : `ASQ_TEST_IMAGE=mcr.microsoft.com/mssql/server:2022-latest go test -tags=integration ./tests/integration -run TestSessionTLS -v -count=1` ; attendu : 1 test.
- [ ] Harness : génération crypto/rand de mots de passe, fichier env temporaire 0600 ; `exec.CommandContext` avec argv séparés :

```text
podman run --detach --name asq-test-<random> --label io.argosql.test=<run-id> --publish 127.0.0.1::1433 --env-file <temp-file> <image>
podman port <container-id> 1433/tcp
podman inspect --format {{.Image}} <container-id>
```

Le nom aléatoire vient du harness, jamais d'un nom SQL. Le fichier contient ACCEPT_EULA=Y, MSSQL_PID=Developer, MSSQL_SA_PASSWORD généré. Attendre PingContext réussi dans 90 s, puis créer AppDB avec Query Store via bootstrap.sql :

```sql
ALTER DATABASE AppDB SET QUERY_STORE = ON;
ALTER DATABASE AppDB SET QUERY_STORE
(OPERATION_MODE = READ_WRITE, QUERY_CAPTURE_MODE = ALL,
 INTERVAL_LENGTH_MINUTES = 1, DATA_FLUSH_INTERVAL_SECONDS = 60);
```

Après une charge, l'admin exécute `EXEC sys.sp_query_store_flush_db` dans AppDB, puis attend la présence du marqueur dans une boucle bornée à 30 s plutôt qu'un sleep fixe. Les tests de frontières temporelles utilisent des fixtures SQL synthétiques. TestMain construit une fois le binaire quand cmd/asq existe à partir de la tâche 9 ; avant cela les tests de session utilisent directement sqlserver.Open. Échec de build=échec de suite, jamais skip. Pour la readiness, chaque tentative a son propre délai court. `t.Cleanup` ferme les clients et supprime par ID le conteneur créé. Ne pas inspecter/imprimer son environnement. Les tests sans build tag ne démarrent rien ; avec tag et prérequis absents, échec explicite, pas skip silencieux.
- [ ] Créer certificats CA/serveur via crypto/x509 dans t.TempDir, SAN pour localhost/127.0.0.1 ; variantes expirée et mauvais SAN. Monter certificat/clé et mssql.conf (`network.tlscert`, `network.tlskey`, `network.forceencryption`) dans le conteneur TLS dédié, avec permissions lisibles par son UID SQL. Faire la préparation des fichiers dans un volume dédié au run si les UID rootless diffèrent ; ne pas modifier les permissions d'un fichier utilisateur.
- [ ] Vert sur 2019 et 2022 : défaut true/absent chiffré, false+CA valide réussi, false+CA inconnue/mauvais nom/expiration ->3. Conflit de verrou réel via connexion admin, timeout environ 5 s avec tolérance CI, deadline globale 1 s et interruption. Relever version moteur, pilote et ID image dans le compte rendu sans credentials.
- [ ] Commit : `test: verify held sessions and TLS against isolated SQL Server`.

### Tâche 5 : cellules et formats complets

**Fichiers :** créer `internal/output/cell.go`, `scan.go`, `tsv.go`, `json.go`, `decode.go` et leurs tests.

**Interfaces :** `EncodeTSVCell(model.Cell) string` ; le convertisseur des valeurs du pilote, dont dépendent les quatorze diagnostics et qui n'appartenait à aucune tâche :

```go
// scan.go : seul endroit du projet qui transforme une ligne SQL en []model.Cell.
func ScanRow(rows *sql.Rows, types []*sql.ColumnType) ([]model.Cell, error)
```

Mesuré avec `go-mssqldb` v1.11.0 : `decimal`, `money`, `varbinary` et `uniqueidentifier` arrivent tous en `[]uint8`, et `datetime2`/`datetimeoffset` en `time.Time`. Aucun de ces types n'est admis par `Cell.Value`, dont le domaine est nil, string, bool, int64, float64. Seul `ColumnType.DatabaseTypeName()` sépare un décimal exact d'un binaire : la conversion se décide sur ce nom, jamais sur le type Go seul, sinon un `varbinary` devient un nombre en texte. Règles : decimal et money en string exacte, varbinary en hexadécimal préfixé, uniqueidentifier en forme canonique, date et heure en texte ISO 8601, bigint en int64 puis chaîne JSON. Un type SQL non couvert est une erreur explicite, jamais un `fmt.Sprintf` de secours. Tester chacun de ces types contre un serveur réel et non seulement contre des valeurs fabriquées.

Encodeur incrémental aligné sur le modèle push du collecteur :

```go
func NewTableEncoder(w io.Writer,format string,spec model.TableSpec)(*Encoder,error)
func (e *Encoder) WriteRow(row []model.Cell) error
func (e *Encoder) Close() error
func NewTableDecoder(r io.Reader,format string,spec model.TableSpec)(*Decoder,error)
func (d *Decoder) Next()([]model.Cell,error) // io.EOF à la fin
```

Begin crée Encoder, Row appelle WriteRow, End appelle Close ; aucune goroutine/channel d'adaptation. decode.go est le lecteur inverse explicite nécessaire à la tâche 7 : JSON via json.Decoder token/ligne, TSV via lecteur buffered et machine d'échappement inverse. Il lit au plus une ligne à la fois, conserve les types/colonnes positionnels et rejette un artefact mal formé. Tester encode->decode sur tous les types et échappements ci-dessous, y compris `\N` littéral versus null. Ne jamais json.Unmarshal l'artefact entier.

- [ ] Test :

```go
func TestTSVEscapes(t *testing.T) {
    cases := []struct{ c model.Cell; want string }{
        {nil, `\N`}, {"", ""},
        {`\N`, `\\N`}, {"a\tb\nc", `a\tb\nc`},
    }
    for _, c := range cases { if got:=EncodeTSVCell(c.c); got!=c.want { t.Fatalf("%q != %q",got,c.want) } }
}
```

- [ ] Rouge : `go test ./internal/output -v`.
- [ ] Encoder null avant les chaînes ; échapper dans l'ordre backslash, tab, CR, LF. JSON écrit columns une fois puis rows positionnelles ; bigint/decimal sont convertis en chaînes avant encodage. Scan SQL conserve les decimal exacts en texte via conversion SQL explicite lorsque le pilote ne garantit pas le type exact ; tester 38 chiffres, pas un float64 intermédiaire. En-têtes doublons conservés. Documenter les types pris en charge ; date/time en texte ISO, binary en hex.

```go
strings.NewReplacer("\\", "\\\\", "\t", "\\t", "\r", "\\r", "\n", "\\n").Replace(text)
```

- [ ] Vert : round-trip Unicode, CRLF, bigint 9223372036854775807, decimal(38,4), noms de colonnes spéciaux ; writer défaillant propagé ; `go test ./internal/output -v`.
- [ ] Commit : `feat: serialize exact SQL values as TSV and positional JSON`.

### Tâche 6 : artefacts et plafonds de collecte

**Fichiers :** créer `internal/artifacts/store.go`, `collector.go`, `manifest.go` et tests associés ; modifier .gitignore pour `/bin/`, `/dist/`, `/test-results/` uniquement.

**Interfaces :** consomme model.Sink et output.NewTableEncoder ; produit :

```go
type Limits struct { Rows, Bytes int64 }
func New(dir, format string, limits Limits) (*Collector, error)
// Collector implémente model.Sink.
func (c *Collector) Finish(info model.ContextInfo, runErr error) (model.Result,error)
```

- [ ] Tester la frontière exacte puis une ligne supplémentaire :

```go
func TestRowLimit(t *testing.T) {
    c, err:=New(t.TempDir(),"json",Limits{Rows:2,Bytes:1048576}); if err!=nil { t.Fatal(err) }
    if err=c.Begin(model.TableSpec{Name:"x",Columns:[]model.Column{{Name:"n",SQLType:"int"}}}); err!=nil {t.Fatal(err)}
    for i:=0;i<2;i++ { if err=c.Row([]model.Cell{{Value:int64(i)}}); err!=nil {t.Fatal(err)} }
    err=c.Row([]model.Cell{{Value:int64(2)}})
    if model.ExitCode(err)!=7 {t.Fatalf("limit error: %v",err)}
}
```

- [ ] Rouge : `go test ./internal/artifacts -v`.
- [ ] Store crée un sous-répertoire d'invocation unique, fichiers générés via O_EXCL, répertoires 0700/fichiers 0600 sous Unix. Quota global inclut tous les fichiers, manifeste et fichiers temporaires conservés. Réserver 64 KiB pour le manifeste ; si celui-ci réclame davantage, retirer uniquement les fichiers temporaires non finalisés créés par ce run avant la fin, jamais les artefacts déjà complets ou échouer 6 sans prétendre complet. Manifestes n'embarquent ni valeurs SQL ni liste illimitée de warnings. Le manifeste sérialise `model.Completeness` et jamais `model.PreviewState` : au moment où le collecteur écrit, l'aperçu n'a pas encore été calculé, et sérialiser ses champs à zéro affirmerait qu'aucune ligne n'a été affichée. Un test lit le manifeste produit et échoue s'il contient les clés `rows_shown`, `preview_complete` ou `omitted_reasons`.

```go
// Un objet source indivisible est copié avec une sentinelle de dépassement.
n, err := io.Copy(dst, io.LimitReader(src, remaining+1))
if n > remaining { /* supprimer seulement ce temporaire créé ici ; retourner PublicError Code 7 */ }
```

Pour une table interrompue, fermer proprement le JSON/TSV des lignes acceptées et marquer partiel. Pour XML/module dépassé, aucun fichier final tronqué. À exactement N lignes, End(true,...) reste complet ; seule la N+1e démontre le dépassement. TOP N intentionnel est complet. Finish reçoit l'erreur de collecte et la conserve, sauf erreur fichier ultérieure ->6.
- [ ] Vert : limites multi-tables, octets avant/à/après cap, quota source unique, collision/symlink, échec disque injecté par writer, nettoyage local, UTF-8 sans BOM et sortie manifeste. Tester Windows pour permissions/rename distinctement à la tâche 16.
- [ ] Commit : `feat: stream diagnostic artifacts with explicit collection limits`.

### Tâche 7 : aperçu borné et erreurs sérialisées

**Fichiers :** créer `internal/output/preview.go`, `preview_test.go`. Ne modifier ni collector.go ni result.go : depuis que l'aperçu appartient entièrement à Render et que PreviewState est défini à la tâche 1, cette tâche n'a plus de raison de toucher aux tâches 1 et 6. Si elle croit en avoir besoin, c'est un signal de défaut à remonter, pas une permission.

**Interfaces :** `PreviewOptions{Rows int; CellLimit int; NoTruncate bool; ByteLimit int}` ; `Render(result model.Result, options PreviewOptions, format string) ([]byte,error)`.

Le collecteur garde sur disque les données complètes. Ni `New(dir, format, limits)` ni `Finish(info, runErr)` ne transporte `PreviewOptions`, et cette tâche s'interdit de changer ces signatures : le collecteur ne peut donc pas décider seul du nombre de candidats. Trancher ainsi, sans toucher aux signatures existantes : le collecteur n'a aucune responsabilité d'aperçu, et `Render` fait la seconde lecture des artefacts en utilisant le décodeur de la tâche 5, à partir des chemins que `Finish` a déjà placés dans `model.Result.Artifacts`. La phrase qui confiait au collecteur la construction des candidats venait de la correction de la relecture précédente et se contredisait elle-même. La tâche 5 fournit NewTableDecoder/Next pour cette seconde lecture ; ne pas introduire un format privé supplémentaire. Render retient au plus le nombre de lignes demandé et les octets potentiellement affichables, jamais 10,000 cellules de 100 MiB en RAM, et mesure les octets réellement encodés. Le collecteur fixe `model.Completeness` : rows_collected, collection_complete et properties_complete. Render travaille sur une copie du résultat, remplit `model.PreviewState` sur cette copie, puis la sérialise ; le manifeste écrit par le collecteur ne connaît pas ces champs et ne peut donc pas mentir sur l'aperçu.

Réserve mémoire de la seconde lecture : au plus ByteLimit octets sérialisables retenus par table. Une ligne dont le décodage dépasse à elle seule cette réserve n'est pas écartée : elle est retenue sous forme tronquée à la réserve, avec sa raison dans omitted_reasons. Écarter la ligne transformerait `--no-truncate` sur une grosse cellule en aperçu vide, ce qui est exactement la confusion entre omis et vide que cette tâche existe pour empêcher. La première ligne candidate de chaque table non vide est toujours retenue, au moins tronquée. Le plafond d'octets de la sortie s'applique inchangé ensuite ; `--no-truncate` porte sur la troncature par runes, jamais sur le plafond. L'utilisation mémoire d'une ligne décodée reste distincte de celle du fichier complet.

- [ ] Test directeur :

```go
func TestOversizedCellIsNotEmpty(t *testing.T) {
    r:=model.Result{SchemaVersion:1,OK:true,Tables:[]model.TableResult{{
        Spec:model.TableSpec{Name:"x",Columns:[]model.Column{{Name:"text",SQLType:"nvarchar(max)"}}},
        Rows:[][]model.Cell{{{Value:strings.Repeat("x",40000)}}},
        State:model.Completeness{RowsCollected:1,CollectionComplete:true,PropertiesComplete:true},
    }}}
    b,err:=Render(r,PreviewOptions{Rows:10,NoTruncate:true,ByteLimit:32768},"json")
    if err!=nil || !json.Valid(b) || len(b)>32768 { t.Fatalf("bad preview: %v",err) }
    if !bytes.Contains(b,[]byte("preview_omitted")) {t.Fatal("must distinguish omitted from empty")}
}
```

- [ ] Rouge : `go test ./internal/output -run 'TestOversizedCellIsNotEmpty|TestPreviewBudget' -v` ; attendu : 2 tests. Le filtre nomme les tests, il n'utilise pas `Test.*Preview`, qui masque une absence derrière une correspondance large.
- [ ] Appliquer cet ordre : tronquer cellules par runes -> réserver enveloppe/métadonnées -> parts égales entre tables non vides -> lignes entières -> redistribuer dans l'ordre de la spec -> réencoder et vérifier la taille incluant newline. Retirer la dernière ligne candidate si le calcul exact augmente avec les métadonnées. Gérer fallback manifeste puis erreur fixe sans chemin. Ce littéral se sérialise depuis `model.FallbackError` défini à la tâche 1, jamais depuis `model.Result` : mesuré, `Result` produit en plus `context`, `tables` et `sql_number`, et `omitempty` ferait disparaître `"ok":false`. Les raisons de réduction viennent du vocabulaire fermé de la tâche 1 ; ne pas inventer de chaîne locale.

```json
{"schema_version":1,"ok":false,"error":{"code":6,"kind":"output_budget","message":"response metadata exceeds output budget"}}
```

- [ ] Vert : trois sections, zéro ligne demandée, marqueur littéral dans la donnée, cellules Unicode, métadonnées trop grandes, chemin trop long, erreur 7 avec artefacts partiels ; stdout JSON unique et stderr séparé. Fixture dorée pour TSV et JSON d'une même réponse. Assertion explicite sur la ligne surdimensionnée : le rendu de `TestOversizedCellIsNotEmpty` porte `rows_shown` valant 1 et une cellule non vide, pas une table à zéro ligne. Un aperçu vide fait échouer ce test ; c'est le défaut que la réserve par table avait introduit.
- [ ] Commit : `feat: bound stdout without confusing omitted and empty results`.

### Tâche 8 : résolution d'objets et capacités à trois états

**Fichiers :** créer `internal/sqlserver/permissions.go`, `objects.go` et tests ; créer `tests/integration/permissions_test.go`, `tests/integration/sql/principals.sql`, `tests/integration/sql/objects.sql`.

**Interfaces :** consomme Session.Conn ; produit :

```go
type Permission int
const (Unknown Permission = iota; Allowed; Denied)
type Object struct { ID int64; Schema, Name, Type string }
// Sonde niveau objet. name est déjà résolu par Resolve : voir la note mesurée.
func Probe(ctx context.Context, conn *sql.Conn, name, class, permission string) (Permission,error)
// Sonde niveau instance. Elle n'a pas de securable et s'écrit obligatoirement
// HAS_PERMS_BY_NAME(NULL, NULL, @permission).
func ServerProbe(ctx context.Context, conn *sql.Conn, permission string) (Permission,error)
func Resolve(ctx context.Context, conn *sql.Conn, qualified string) (Object,error)
```

- [ ] Cas rouge d'ambiguïté dans le faux pilote et la fixture Q :

```go
func TestUnknownIsNotDenied(t *testing.T) {
    // permissionFromSQL est le convertisseur privé de permissions.go.
    if permissionFromSQL(sql.NullInt64{}) != Unknown {t.Fatal("NULL lost")}
    if permissionFromSQL(sql.NullInt64{Int64:0,Valid:true}) != Denied {t.Fatal("zero")}
}
```

Ce test unitaire ne prouve à lui seul rien du moteur : il vérifie un
convertisseur sur des valeurs fabriquées. Mesuré, `HAS_PERMS_BY_NAME` ne rend
NULL que sur une sonde malformée, donc `Unknown` ne peut pas venir d'un état
légitime du serveur. Le test qui porte la garantie est celui-ci, à écrire dans
`tests/integration/permissions_test.go`, et il énumère les sondes que le
registre émet réellement :

```go
func TestEveryProbeIsWellFormed(t *testing.T) {
    // AllProbes(major) est exporté par internal/sqlserver : la liste exacte des
    // couples (classe, permission) que les commandes émettent, plus les
    // permissions instance de ServerProbe. Elle dépend de la version, la
    // permission instance différant entre Major=15 et Major>=16. Chaque entrée
    // porte Label et une méthode Run(ctx, conn, name) (Permission, error).
    lab := NewLab(t, os.Getenv("ASQ_TEST_IMAGE"))
    ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
    defer cancel()
    sess, err := sqlserver.Open(ctx, lab.Profile)
    if err != nil { t.Fatal(err) }
    defer sess.Close()
    for _, pr := range sqlserver.AllProbes(sess.Major) {
        got, err := pr.Run(ctx, sess.Conn, "dbo.Orders")
        if err != nil { t.Fatalf("%s: %v", pr.Label, err) }
        if got == sqlserver.Unknown {
            t.Fatalf("%s rend NULL sur un serveur réel : sonde malformée", pr.Label)
        }
    }
}
```

Une sonde qui rend `Unknown` ici est un défaut de code, pas un fait sur le
principal. Ajouter un cas négatif volontaire (classe `SERVER`, invalide) et
vérifier qu'il rend bien `Unknown`, pour que le test échoue si quelqu'un fait
disparaître la distinction.

- [ ] Rouge : `go test ./internal/sqlserver -run 'TestUnknownIsNotDenied|TestResolveTwoPart' -v` ; attendu : 2 tests. C'est ce filtre qui a servi de démonstration : avec `TestResolve` non défini, il rendait `ok` et sortait 0.
Comportement mesuré de `HAS_PERMS_BY_NAME`, sur SQL Server 2022 RTM-CU26 (16.0.4265.3), à respecter tel quel :

| Situation | Retour | Ce que le code en conclut |
| --- | --- | --- |
| Objet existant, principal sans droit | `0` | Denied |
| Objet inexistant | `0` | Denied, d'où la nécessité de résoudre d'abord |
| Permission inconnue de SQL Server | `NULL` | sonde malformée, défaut de code |
| Classe `'SERVER'` | `NULL` | sonde malformée, défaut de code |
| `HAS_PERMS_BY_NAME(NULL, NULL, 'VIEW SERVER STATE')` | `1` | forme correcte au niveau instance |

La fonction rend donc deux valeurs et non trois. Elle ne dit jamais « je ne peux
pas prouver » : elle rend `NULL` uniquement quand la sonde elle-même est mal
écrite. Le tri-état que la spec exige est porté par la résolution, pas par la
sonde : zéro ligne dans `sys.objects` vaut `not_found_or_not_visible`, parce que
le catalogue n'expose pas ce qui est invisible au principal. Ne pas présenter
`Unknown` à l'utilisateur comme un fait sur ses droits ; le mapper en code 5,
kind `probe_malformed`, et le traiter comme un défaut à corriger.

Conséquence sur l'ordre des codes : puisqu'un objet inexistant sonde à `Denied`,
l'ordre « 8 avant 4 » n'est pas une préférence de présentation. C'est la seule
chose qui empêche l'outil de répondre « permission refusée » sur un objet qui
n'existe pas. Écrire un test qui sonde un nom absent sans résoudre d'abord et
constate le `0`, pour que la raison de l'ordre reste visible dans la suite.

Une sonde `SELECT` au niveau OBJECT ne mesure pas non plus ce que valent les
droits d'une commande qui lit des vues système : un principal avec un
`GRANT SELECT` de colonne sonde à `0` sur la table. Ne pas fermer une commande
sur cette seule base ; sonder la permission que la commande utilise réellement.

- [ ] Probe rend Allowed ou Denied et traite NULL comme un défaut ; ServerProbe utilise obligatoirement la forme `(NULL, NULL, @permission)`, avec VIEW SERVER STATE sur Major=15 et VIEW SERVER PERFORMANCE STATE sur Major>=16 conformément à la spec. Resolve utilise sys.schemas JOIN sys.objects avec paramètres distincts. Décomposer les noms à deux parties en respectant les crochets et `]]`, espaces/dots à l'intérieur des identifiants ; rejeter trois parties et noms mal formés avec 2. Ne pas construire du SQL à partir d'un identifiant utilisateur.

```sql
SELECT o.object_id, s.name, o.name, o.type
FROM sys.objects AS o
JOIN sys.schemas AS s ON s.schema_id=o.schema_id
WHERE s.name=@schema AND o.name=@name;
```

Zéro ligne ->8 `not_found_or_not_visible` par défaut. Permission connue refusée sur objet résolu ->4. Le champ NULL d'une sonde reste unknown. Ne pas exiger VIEW DEFINITION pour résoudre un objet déjà visible grâce à un autre droit.
- [ ] Créer Q/I/S avec mots de passe transmis par le harness, pas littéraux versionnés. Appliquer les bundles exacts de la spec et des variantes metadata-only/SELECT-colonne. Sous Q, Resolve(dbo.Orders) renvoie 8 ; sous I, objet résolu ; sous I, sonde instance=Denied ; sous S=Allowed. Tester module chiffré, schéma avec DENY, objet absent et nom avec crochets.
- [ ] Vert : `go test ./internal/sqlserver -v`, puis `go test -tags=integration ./tests/integration -run 'TestPermissions|TestEveryProbeIsWellFormed' -v -count=1` ; attendu : 2 tests ; sur les deux images.
- [ ] Commit : `feat: resolve visible objects without inventing permission facts`.

### Tâche 9 : CLI utilisable, info et santé Query Store

**Deux unités de revue successives :** 9a termine parseur/registre/main et help offline ; 9b ajoute info, health/status et les tests SQL. Faire un cycle rouge/vert et commit séparé pour chaque unité.

**Fichiers :** créer `cmd/asq/main.go`, `internal/cli/parse.go`, `registry.go`, `run.go`, `parse_test.go`, `run_test.go`, `internal/diagnostics/info.go`, `health.go`, `health_test.go`, `sql/info.sql`, `sql/health.sql`, `sql/coverage.sql`, `embed.go`, `tests/integration/status_test.go`.

**Interfaces :**

```go
// diagnostics
func Info(ctx context.Context, s *sqlserver.Session, dst model.Sink) error
type Health struct { Desired, Actual string; ReadOnlyReason int64; HasHistory bool }
func ReadHealth(ctx context.Context, s *sqlserver.Session) (Health,error)
func Status(ctx context.Context, s *sqlserver.Session, dst model.Sink) error
func DecodeReadOnly(h Health) []string
// cli
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int
```

- [ ] Émettre l'avis de compatibilité non validée dans `Run`, juste après `sqlserver.Open` et avant de dispatcher la commande, quand `s.Major` vaut 17 : `model.Notice{Kind: "unvalidated_version"}`. C'est `Run` qui le porte et non `Open`, parce que `Session` n'a pas d'accès au `Sink`, et c'est `Run` et non `Info` parce que l'avis vaut pour toutes les commandes et pas seulement pour `info`. Test : `Run` sur un serveur factice de Major 17 produit exactement un avis de cette sorte, et zéro sur Major 16.
- [ ] Écrire le test offline et le test du reason=0 :

```go
func TestHelpOffline(t *testing.T) {
    var out,errout bytes.Buffer
    code:=Run(context.Background(),[]string{"help","--json"},&out,&errout)
    if code!=0 || !json.Valid(out.Bytes()) {t.Fatalf("%d %s",code,errout.String())}
}
// Dans diagnostics/health_test.go :
func TestReasonZero(t *testing.T) {
    got:=DecodeReadOnly(Health{Desired:"READ_WRITE",Actual:"READ_WRITE"})
    if len(got)!=1 || got[0]!="none" {t.Fatal(got)}
}
```

- [ ] Rouge : `go test ./internal/cli ./internal/diagnostics -v`.
- [ ] Registre unique `Command{Name string; Tables []model.TableSpec; Flags []Flag; Execute func(context.Context,*sqlserver.Session,Request,model.Sink)error}` ; `Flag{Name,Kind string; Default any; Min,Max int64}` ; `Request` est défini ainsi :

```go
type Request struct {
    Command, ContextName, Database, ConfigPath, Format, OutDir string
    Object, Table, By, Aggregate, Since, Until string
    QueryID, PlanID, MinExecutions int64
    TimeoutSeconds, PreviewRows, TruncateRunes, Hours, Top int
    NoTruncate, IncludeInternal, Summary bool
    Explicit map[string]bool
}
```

parse.go possède les défauts de chemins. `os.UserConfigDir` et `os.UserCacheDir` rendent deux valeurs, donc l'écriture imbriquée ne compile pas ; c'est justement l'erreur de résolution que le plan veut mapper vers 2 :

```go
dir, err := os.UserConfigDir()
if err != nil { return &model.PublicError{Code: 2, Kind: "config_path", Message: "cannot resolve user config directory"} }
req.ConfigPath = filepath.Join(dir, "argosql", "config.yaml")
```

Même forme pour OutDir avec `os.UserCacheDir`. Différer la résolution des deux pour help, qui ne doit pas échouer sur un environnement sans répertoire de configuration. Flags globaux : --ctx requis pour SQL, --db optionnel si profil renseigné, --config, --format tsv|json (tsv), --timeout 1–300 (30), --preview 0–10000 (10), --truncate 1–10000 (200), --no-truncate bool (false), --out-dir. Explicit mémorise la présence afin de refuser --no-truncate avec --truncate même égal au défaut. Les flags spécifiques sont admis uniquement sur leur commande ; hours/since/until sont mutuellement exclus selon la spec. Aucun flag par commande n'était nommé dans ce plan avant cette révision ; les voici, tirés de la spec, avec leurs bornes, à porter dans le registre au fur et à mesure des tâches 10 à 14 :

| Flag | Commandes | Type et bornes | Défaut |
| --- | --- | --- | --- |
| `--by` | qs top | énum cpu\|duration\|reads\|executions | cpu |
| `--aggregate` | qs top | énum total\|avg ; avg rejeté avec 2 pour executions | total |
| `--top` | qs top, idx missing | entier 1–100 | 10 |
| `--min-executions` | qs top | entier >= 1 | 1 |
| `--object` | qs top | schema.name d'un module parent ; table résolue rejetée avec 2 | vide |
| `--include-internal` | qs top | booléen | false |
| `--hours` | qs top, qs query | entier positif, exclusif avec since/until | 24 |
| `--since` / `--until` | qs top, qs query | RFC3339, les deux ensemble, since < until | vide |
| `--plan-id` | plan | entier signé 64 bits positif, requis | aucun |
| `--summary` | plan | booléen | false |
| `--table` | idx missing | schema.name, suit le contrat de visibilité | vide |

`Flag{Name,Kind string; Default any; Min,Max int64}` ne peut exprimer ni une énumération ni une exclusion mutuelle. Ajouter `Enum []string` et `ExclusiveWith []string`, et une méthode de validation qui les applique, plutôt que de disperser ces règles dans chaque commande. `Command` reçoit de même les champs que `help` doit rendre selon la spec : exemples, unités, permissions requises et versions concernées. Un registre qui ne les porte pas oblige `help` à les réinventer. Tester chacun des défauts/limites et les deux placements des flags. Parser à partir du registre, acceptant flags avant/après le chemin et après les positionnels ; consommer la valeur d'un flag avant de chercher un sous-chemin. Rejeter flags inconnus, doublons contradictoires, positionnels supplémentaires. Ne pas choisir une commande via un préfixe ambigu. `help --json` ne lit ni config ni connexion. Registre initial : help, info, qs status seulement.
- [ ] Info : SERVERPROPERTY, DB_NAME, USER_NAME et compatibility_level. Health : sys.database_query_store_options ; couverture par MIN(start_time)/MAX(end_time) restreint aux intervalles portant réellement des lignes de runtime, EXISTS runtime global. Mesuré sur une base fraîche de 2022 : un intervalle existe déjà avec zéro ligne de runtime, si bien qu'un MIN/MAX non restreint annonce une fenêtre de couverture d'une heure alors que has_history vaut 0. Restreinte aux intervalles porteurs de lignes, la même couverture rend NULL. Retourner NULL pour oldest_interval et newest_interval quand il n'y a pas d'historique, jamais une fenêtre inventée, et tester exactement ce cas sur une base créée à l'instant. Champs dans status : desired_state, actual_state, readonly_reason, decoded reasons, capture_mode, current/max_storage_mb, retention_days, interval_minutes ; coverage : oldest/newest_interval, has_history. État OFF/ERROR n'empêche pas status de retourner 0. Le mapping des bits vient de la documentation de sys.database_query_store_options, testé sur bits connus et inconnus, et ne dépend pas de SELECT *.

```go
if h.ReadOnlyReason == 0 {
    if h.Actual=="READ_ONLY" && h.Desired=="READ_ONLY" {return []string{"configured_read_only"}}
    if h.Actual=="READ_WRITE" {return []string{"none"}}
    return []string{"no_reason_reported"}
}
```

Run crée le contexte global, ouvre Session, collecte, finalise, rend puis écrit stdout une fois. En cas d'erreur, finalise les artefacts partiels et produit le code définitif. `main` utilise signal.NotifyContext ; l'interruption devient 130. Séparer expiration interne et signal utilisateur. Sortie stderr reformulée et redaction de la valeur du secret.
- [ ] Vert : parsing exemples exacts de la spec, limites des flags, erreurs JSON avant connexion, santé réelle OFF/READ_ONLY et permissions refusées ; `go test ./...` puis test intégration `TestStatus` (à créer dans `tests/integration/status_test.go`).
- [ ] Commit : `feat: expose offline discovery and database health commands`.

**Point de revue tranche 1 :** utiliser `go run ./cmd/asq help --json`, puis info/qs status sur une fixture. Les deux TLS et les quotas doivent déjà être testés ; ne pas reporter ces fondations à la fin.

## Tranche 2 — Query Store et plans

### Tâche 10 : classement Query Store exact

**Fichiers :** créer `internal/diagnostics/window.go`, `top.go`, `top_test.go`, `window_test.go`, `sql/top_2019.sql`, `sql/top_2022.sql`, `tests/integration/top_test.go`, `tests/integration/sql/workload.sql` ; modifier le registre CLI.

**Interfaces :**

```go
type Window struct { Since, Until time.Time }
type TopOptions struct { Window Window; By, Aggregate, Object string; Top int; MinExecutions int64; IncludeInternal bool }
func ParseWindow(now time.Time, hours int, since, until string) (Window,error)
func Top(ctx context.Context, s *sqlserver.Session, opts TopOptions, dst model.Sink) error
```

- [ ] Fixture de calcul exacte : deux lignes même plan/intervalle avec (count=1, avg_cpu_us=1000) et (count=9, avg_cpu_us=3000). Attendu executions=10, cpu_total_ms=28, cpu_avg_ms=2.8. Ajouter autre plan même query, autre replica_group à conserver comme ligne distincte ; execution_type 3 et 4 à exclure. Écrire un test de fenêtre :

```go
func TestWindow(t *testing.T) {
    now:=time.Date(2026,9,8,12,0,0,0,time.UTC)
    w,err:=ParseWindow(now,24,"","")
    if err!=nil || !w.Since.Equal(now.Add(-24*time.Hour)) || !w.Until.Equal(now) {t.Fatal(w,err)}
}
```

- [ ] Rouge : `go test ./internal/diagnostics -run 'TestWindow|TestTopAggregation' -v` ; attendu : 2 tests.
- [ ] Esquisse du noyau SQL, à étendre selon la prose qui suit le bloc. Adapter le nom de métrique par liste statique et jamais par texte libre. Assertion qui prouve l'extension : le jeu de colonnes rendu par `top` compte les neuf colonnes nommées plus bas, `top_2022.sql` groupe et retourne `replica_group_id`, et `top_2019.sql` retourne cette colonne en NULL typé. Un test compare la liste des colonnes du TableSpec à la liste attendue et échoue si l'une manque.

```sql
WITH runtime AS (
  SELECT rs.plan_id, rs.runtime_stats_interval_id,
         SUM(CONVERT(bigint,rs.count_executions)) AS executions,
         SUM(rs.avg_cpu_time * rs.count_executions) AS cpu_us
  FROM sys.query_store_runtime_stats AS rs
  JOIN sys.query_store_runtime_stats_interval AS i
    ON i.runtime_stats_interval_id=rs.runtime_stats_interval_id
  WHERE rs.execution_type=0 AND i.start_time<@until AND i.end_time>@since
  GROUP BY rs.plan_id,rs.runtime_stats_interval_id
)
SELECT TOP (@top) q.query_id,
       SUM(r.executions) AS executions,
       SUM(r.cpu_us)/1000.0 AS cpu_total_ms,
       SUM(r.cpu_us)/NULLIF(SUM(r.executions),0)/1000.0 AS cpu_avg_ms
FROM runtime AS r
JOIN sys.query_store_plan AS p ON p.plan_id=r.plan_id
JOIN sys.query_store_query AS q ON q.query_id=p.query_id
WHERE (@include_internal=1 OR q.is_internal_query=0)
  AND (@object_id IS NULL OR q.object_id=@object_id)
GROUP BY q.query_id
HAVING SUM(r.executions)>=@min_executions
ORDER BY cpu_total_ms DESC,q.query_id;
```

Étendre cette même agrégation avec duration et logical reads, pas une requête par métrique. Déclarer IDs/executions comme bigint (chaînes JSON), les métriques calculées comme float SQL (nombres JSON). Le test 2.8 utilise une tolérance absolue de 1e-9 et le total 28 une tolérance de 1e-9, jamais une comparaison textuelle du float ; le test executions=10 reste exact. Les totaux de lectures calculés à partir de moyennes sont des estimations flottantes, pas des compteurs entiers inventés. Ce choix ne change pas la préservation exacte des decimal de la base à la tâche 5. Sur 2022, grouper rs.replica_group_id à chaque étage, le porter dans le GROUP BY final et dans le départage de l'ORDER BY, et retourner cette colonne. 2019 retourne NULL typé pour elle. Le bloc esquissé plus haut finit sur `GROUP BY q.query_id` et `ORDER BY cpu_total_ms DESC, q.query_id` : c'est la forme 2019, et la recopier telle quelle dans top_2022.sql perd le classement par réplique que la spec impose. Colonnes queries : query_id, replica_group_id, executions, cpu_total_ms, cpu_avg_ms, duration_total_ms, duration_avg_ms, reads_total, reads_avg ; pas de texte intégral dans le classement. Contrôle parent_module via Resolve et type avant exécution.
- [ ] Réutiliser ReadHealth. Dans état non collectant sans historique ->4, sinon fenêtre sans correspondance ->table vide 0. Ajouter warnings capture/coverage sans prétendre exhaustivité. Vérifier `executions/avg` refusé avant connexion.
- [ ] Vert : fixtures SQL synthétiques dans tables temporaires de test pour vérifier le noyau d'agrégation (ne pas écrire dans les catalogues), puis charge réelle Query Store. Le test synthétique remplace uniquement la source de données du noyau SQL ; les attentes sont numériques fixes. Sur charge réelle, découvrir les IDs via marqueur unique et attendre/forcer le flush avec l'admin. Ce mécanisme de découverte appartient à cette tâche et s'expose comme helper du harnais, `func (lab *Lab) QueryID(t *testing.T, marker string) int64`, ajouté à `tests/integration/fixture_test.go` : il exécute la charge portant le marqueur, force le flush, attend la présence dans une boucle bornée à 30 s et rend le query_id. La tâche 11 s'en sert et ne le redéfinit pas ; aucun ID codé en dur nulle part. `go test -tags=integration ./tests/integration -run TestTop -v -count=1` ; attendu : 1 test ; sur 2019/2022.
- [ ] Commit : `feat: rank Query Store workloads with weighted metrics`.

### Tâche 11 : détail query_id et export texte

**Fichiers :** créer `internal/diagnostics/query.go`, `query_test.go`, `sql/query.sql`, `sql/query_plans_2019.sql`, `sql/query_plans_2022.sql`, `tests/integration/query_test.go` ; modifier registre.

**Harness à compléter dans fixture_test.go :** `func (lab *Lab) Run(t *testing.T, principal string, args []string) (model.Result,int)` écrit un profil temporaire, injecte seulement le secret de ce principal dans l'environnement enfant et lance `exec.CommandContext` sur un binaire construit une fois dans TestMain (tâche 4). Ajouter --format json, --ctx, --config et --out-dir temporaires. Lire exec.ExitError.ExitCode ; décoder stdout ; garder stderr pour diagnostic nettoyé. Aucun appel direct à cli.Run depuis tests/integration. Les tests de SIGINT et pipe fermé utilisent ce même binaire.

**Interfaces :** `QueryOptions{ID int64; Window Window}` ; `Query(ctx context.Context,s *sqlserver.Session,opts QueryOptions,dst model.Sink) error`.

- [ ] Écrire le test intégration du nom masqué et texte intact ; helper `Lab.QueryID(t, marker string) int64` à créer dans fixture_test.go, avec SELECT paramétré admin sur query_sql_text :

```go
func TestQueryExport(t *testing.T) {
    lab:=NewLab(t,os.Getenv("ASQ_TEST_IMAGE"))
    id:=lab.QueryID(t,"asq-fixture-query")
    // Lab.Run crée config/secret du principal, lance le binaire et décode model.Result.
    r,code:=lab.Run(t,"Q",[]string{"qs","query",strconv.FormatInt(id,10)})
    if code!=0 || len(r.Artifacts)==0 {t.Fatalf("code=%d",code)}
}
```

- [ ] Rouge : `go test -tags=integration ./tests/integration -run TestQueryExport -v -count=1` ; attendu : 1 test.
- [ ] Chercher q/query_text avec LEFT JOIN facultatif pour le module ; query : query_id, object_id, parent_module nullable, is_internal_query, query_hash, text_preview, text_artifact. File export `.sql` complet via Sink.File. Plans : plan_id, replica_group_id, forced, executions et mêmes totaux/moyennes que top. LEFT JOIN des agrégats vers les plans : un plan sans runtime garde zero/null. L'identité interne reste accessible par ID. Ne pas limiter les plans à ceux forcés.

```sql
SELECT q.query_id,q.object_id,q.is_internal_query,qt.query_sql_text
FROM sys.query_store_query AS q
JOIN sys.query_store_query_text AS qt ON qt.query_text_id=q.query_text_id
WHERE q.query_id=@id;
```

- [ ] Vert : ID manquant=8, ID interne réussi, parent module invisible, pas d'exécutions dans fenêtre, long texte Unicode export identique, OFF avec histoire lisible, permission refusée distincte de zéro ligne. `go test ./internal/diagnostics` et tests intégration Query.
- [ ] Commit : `feat: inspect Query Store queries and preserve full SQL text`.

### Tâche 12 : extraction sqlplan et résumé XML

**Fichiers :** créer `internal/plan/xml.go`, `summary.go`, `xml_test.go`, `summary_test.go`, `internal/diagnostics/plan.go`, `sql/plan.sql`, `tests/integration/plan_test.go` ; modifier registre.

**Interfaces :**

```go
// plan
// Sink.File attend un io.Reader et NormalizeXML écrit dans un io.Writer : les
// brancher directement rend une goroutine et un tuyau nécessaires, exactement
// l'incompatibilité push/pull que la relecture précédente avait corrigée
// ailleurs sans l'atteindre ici. Normaliser vers un fichier temporaire du store,
// puis passer ce fichier ouvert à Sink.File. Aucune goroutine.
func NormalizeXML(src io.Reader,dst io.Writer) (normalized bool,err error)
func Summarize(src io.Reader,dst model.Sink) error
// diagnostics
func Plan(ctx context.Context,s *sqlserver.Session,queryID,planID int64,summary bool,dst model.Sink) error
```

- [ ] Test byte-preserving et déclaration :

```go
func TestNormalizeXML(t *testing.T) {
    for _, input:=range []string{`<ShowPlanXML/>`,`<?xml version="1.0" encoding="utf-16"?><ShowPlanXML/>`} {
        var out bytes.Buffer
        changed,err:=NormalizeXML(strings.NewReader(input),&out)
        if err!=nil {t.Fatal(err)}
        if strings.Contains(input,"utf-16") && (!changed || !strings.Contains(out.String(),"utf-8")) {t.Fatal(out.String())}
        if !strings.Contains(input,"utf-16") && out.String()!=input {t.Fatal("source changed")}
    }
}
```

- [ ] Rouge : `go test ./internal/plan -v`.
- [ ] Vérifier queryID/planID ensemble avant File ; plan existant sans runtime exportable. Lire XML du catalogue sans le reformater ; inspecter uniquement le prologue pour NormalizeXML. Résumé via xml.Decoder.Token, stack de contexte limitée à profondeur XML, heap min de cinq opérateurs, références/warnings plafonnées à 100. Lire `RelOp`/`NodeId`/`EstimatedTotalSubtreeCost`, objets/indexes et warnings présents ; ignorer éléments inconnus ; coûts estimés sans addition des sous-arbres. L'XML brut est finalisé avant Summarize ; parse failure=5 garde son chemin.

```sql
SELECT query_plan FROM sys.query_store_plan
WHERE query_id=@query_id AND plan_id=@plan_id;
```

- [ ] Vert : fixture 80 KiB+, 10,000 opérateurs, ordre stable coût/NodeId, plus de 100 références, warning inconnu, élément malformed, déclarations, absence de runtime et mismatch=8. Instrumenter nombre d'opérateurs retenus <=5 et benchmark mémoire sur taille croissante ; la lecture XML n'ajoute pas une représentation complète du document. `go test ./internal/plan -v` puis intégration Plan.
- [ ] Commit : `feat: export Query Store plans and bounded XML summaries`.

**Point de revue tranche 2 :** exécuter status -> top -> query -> plan sous Q. Le XML/texte intégral est dans les artefacts et tous les chiffres retournés affichent unités/fenêtre/réplique.

## Tranche 3 — inspection, preuve complète et livraison

### Tâche 13 : tables, modules, structure d'index et tailles

**Fichiers :** créer `internal/diagnostics/table.go`, `code.go`, `indexes.go`, `size.go`, leurs tests ; créer `sql/table.sql`, `columns.sql`, `indexes.sql`, `module.sql`, `size.sql`, `tests/integration/objects_test.go` ; modifier registre.

**Interfaces :** quatre fonctions, chacune `(ctx context.Context,s *sqlserver.Session,name string,dst model.Sink) error` : `Table`, `Code`, `Indexes`, `Size`. `Table` appelle le même lecteur privé d'index que `Indexes`, sans invoquer le CLI récursivement.

- [ ] Test chiffré via Lab.Run :

```go
func TestEncryptedModule(t *testing.T) {
    lab:=NewLab(t,os.Getenv("ASQ_TEST_IMAGE"))
    r,code:=lab.Run(t,"I",[]string{"obj","code","dbo.EncryptedProc"})
    if code!=4 || r.Error==nil || r.Error.Kind!="encrypted" {t.Fatalf("%d %#v",code,r.Error)}
}
```

- [ ] Rouge : tests unitaires et `go test -tags=integration ./tests/integration -run 'TestEncryptedModule|TestObjects|TestSize' -v -count=1` ; attendu : 3 tests.
- [ ] Schémas de sortie : table(object_id,schema,name,rows nullable) ; columns(ordinal,name,type,max_length,precision,scale,nullable,identity,computed,default_definition,computed_definition) ; indexes(index_id,name,type,keys,includes,filter,unique,disabled) ; module(object_id,name,type,definition_state,line_count,artifact) ; allocations(index_id,partition_number,allocation_type,used_pages,reserved_pages,used_bytes,reserved_bytes). Pour colonnes Unicode longueur SQL en octets signalée explicitement ou convertie en caractères dans un champ distinct ; `max_length=-1` reste max.
- [ ] SQL jointures par IDs, préagréger colonnes de clés/inclusions avant jointure aux index ; ordre key_ordinal et index_column_id ; échapper leurs libellés dans la sortie, jamais concaténer comme SQL exécutable. Module via sys.sql_modules et OBJECTPROPERTYEX(IsEncrypted) lorsque visible : appliquer tous les états de la spec sans transformer NULL en texte vide.
- [ ] Taille :

```sql
SELECT SUM(CASE WHEN index_id IN (0,1) THEN row_count ELSE 0 END) AS rows,
       SUM(used_page_count) AS used_pages,
       SUM(reserved_page_count) AS reserved_pages
FROM sys.dm_db_partition_stats WHERE object_id=@id;
```

Ce bloc ne rend que les totaux : il est l'esquisse du noyau, pas le contenu de `size.sql`. Construire allocations à partir des trois catégories de cette DMV sans jointure multiplicatrice, avec la ventilation par index_id, partition_number et allocation_type que le schéma de sortie nomme. Assertion qui prouve l'extension : sur une table à index clusterisé, deux non clusterisés et une colonne LOB remplie, la table allocations porte plus d'une ligne, ses catégories couvrent les trois valeurs d'allocation_type rencontrées, et la somme de ses used_pages égale le total du bloc ci-dessus. Un test qui ne verrait qu'une ligne d'allocations a collé l'esquisse dans le fichier. Rejeter memory-optimized pour Size=4 ; Table garde métadonnées et warning row_count. Tests heap, clustered+2 NC, partitions, LOB/overflow, columnstore, objet vide, DECIMAL et computed/default ; la somme des catégories égale les totaux, row_count n'est jamais la somme de tous les index.
- [ ] Vert : même matrice Q/I/S et états chiffré/invisible/absent ; taille et index testés indépendamment des presets de permissions. `go test ./...` puis intégration Objects/Size.
- [ ] Commit : `feat: inspect object definitions index structure and allocation sizes`.

### Tâche 14 : usage, index manquants et statistiques partielles

**Fichiers :** créer `internal/diagnostics/usage.go`, `missing.go`, `stats.go`, leurs tests ; `sql/usage.sql`, `missing.sql`, `stats.sql`, `tests/integration/index_stats_test.go` ; modifier registre.

**Interfaces :** `Usage` et `Stats` ont la signature de Table ; `MissingOptions{Table string; Top int}` et `Missing(ctx context.Context,s *sqlserver.Session,opts MissingOptions,dst model.Sink) error`.

- [ ] Tester la distinction manque de données/manque de propriétés :

```go
func TestPartialStatistics(t *testing.T) {
    lab:=NewLab(t,os.Getenv("ASQ_TEST_IMAGE"))
    r,code:=lab.Run(t,"metadata_only",[]string{"stats","list","dbo.Orders"})
    if code!=0 || len(r.Tables)!=1 || r.Tables[0].State.PropertiesComplete {t.Fatalf("%d %#v",code,r.Tables)}
    if r.Tables[0].State.RowsCollected==0 {t.Fatal("OUTER APPLY lost statistics")}
}
```

- [ ] Rouge : `go test -tags=integration ./tests/integration -run 'TestPartialStatistics|TestIndexDMV' -v -count=1` ; attendu : 2 tests.
- [ ] Usage part de sys.indexes LEFT JOIN usage filtré database_id ; colonnes index_id,name,seeks,scans,lookups,updates,last_seek,last_scan,last_lookup,last_update,observation_status. Compteurs NULL si aucune ligne DMV, pas 0 inventé. Ajouter contexte server_start_time et avertissement reset ; jamais verdict unused.
- [ ] Missing :

```sql
SELECT TOP (@top) d.index_handle,d.object_id,s.name AS schema_name,o.name AS object_name,
       d.equality_columns,d.inequality_columns,
       d.included_columns,g.user_seeks,g.user_scans,g.avg_total_user_cost,g.avg_user_impact,
       (CONVERT(float,g.user_seeks)+g.user_scans)*g.avg_total_user_cost*g.avg_user_impact/100.0 AS impact
FROM sys.dm_db_missing_index_group_stats AS g
JOIN sys.dm_db_missing_index_groups AS ig ON ig.index_group_handle=g.group_handle
JOIN sys.dm_db_missing_index_details AS d ON d.index_handle=ig.index_handle
LEFT JOIN sys.objects AS o ON o.object_id=d.object_id
LEFT JOIN sys.schemas AS s ON s.schema_id=o.schema_id
WHERE d.database_id=DB_ID() AND (@object_id IS NULL OR d.object_id=@object_id)
ORDER BY impact DESC,d.index_handle;
```

Les jointures vers sys.objects et sys.schemas sont des LEFT JOIN, comme la spec l'exige : la visibilité des métadonnées ne doit pas retirer silencieusement une ligne de preuve DMV. Un principal qui voit la DMV sans voir l'objet obtient la ligne avec schema_name et object_name à NULL, jamais une ligne en moins. Tester exactement ce cas et échouer si le nombre de lignes change avec les droits de métadonnées. Afficher limitations et index_handle stable, pas pourcentage de couverture inventé. Fixture deuxième base avec object_id possiblement identique : aucune ligne de cette base ne doit fuiter dans le résultat.
- [ ] Stats : le bloc SQL ci-dessous est l'esquisse du noyau et ne rend que six des douze colonnes attendues. L'étendre avec sys.stats_columns pour les colonnes ordonnées, et avec auto_created, user_created, filter_definition et properties_status. Assertion qui prouve l'extension : le TableSpec de `stats list` compte les douze colonnes nommées ci-dessous, et un test échoue si l'une manque. sys.stats + colonnes ordonnées, OUTER APPLY des propriétés. Retourner stats_id,name,columns,rows,rows_sampled,sample_pct,last_updated,modification_counter,auto_created,user_created,filter,properties_status. NULL sample_pct si rows=0 ; last_updated=NULL avec ligne de propriétés existe reste available. Sonder SELECT seulement pour attribuer un refus connu, jamais créer un motif à partir de l'absence seule.

```sql
SELECT st.stats_id,st.name,p.rows,p.rows_sampled,p.last_updated,p.modification_counter
FROM sys.stats AS st
OUTER APPLY sys.dm_db_stats_properties(st.object_id,st.stats_id) AS p
WHERE st.object_id=@id ORDER BY st.stats_id;
```

- [ ] Vert : permissions serveur 2019/2022, Q objet non résolu->8 avant grant serveur, I usage->4, S->0 ; statistiques partiellement autorisées et null légitime. `go test ./...` puis intégration IndexDMV/PartialStatistics.
- [ ] Commit : `feat: expose index evidence and partially visible statistics`.

### Tâche 15 : matrice de bout en bout et injections de panne

**Fichiers :** créer `tests/integration/workflow_test.go`, `errors_test.go`, `exports_test.go`, `matrix_test.go`, `internal/cli/faults_test.go`, `internal/artifacts/faults_test.go` ; compléter fixture_test.go.

**Interfaces :** Lab.Run(t,principal,args) (model.Result,int) est partagé depuis tâche 11. Les tests CLI par sous-processus construisent le binaire une fois par suite ; ils ne lancent pas `go run` pour chaque assertion.

- [ ] Écrire une matrice explicite reproduisant celle de la spec ; cas directeur :

```go
func TestPrincipalMatrix(t *testing.T) {
    lab:=NewLab(t,os.Getenv("ASQ_TEST_IMAGE"))
    for _,tc:=range []struct{p string;args []string;want int}{
        {"Q",[]string{"info"},0},
        {"Q",[]string{"obj","table","dbo.Orders"},8},
        {"I",[]string{"idx","usage","dbo.Orders"},4},
        {"S",[]string{"idx","usage","dbo.Orders"},0},
    } {
        _,code:=lab.Run(t,tc.p,tc.args); if code!=tc.want {t.Fatalf("%s %v: %d",tc.p,tc.args,code)}
    }
}
```

Ajouter chaque commande de la matrice avec IDs découverts, pas codés en dur. help est lancé sans Lab.Run et sans environnement SQL. Admin tente DML/DDL/EXEC sous chaque principal et vérifie les refus sans exposer ces opérations au binaire.
- [ ] Rouge : `go test -tags=integration ./tests/integration -run 'TestPrincipalMatrix|TestWorkflow|TestExports|TestErrors' -v -count=1` ; attendu : 4 tests.
- [ ] Rejouer le workflow complet en CLI, lire les artefacts réels et JSON. Configurer les états Query Store dans des bases fixture séparées. Mesuré : sur 2022, `CREATE DATABASE` seul donne déjà `actual_state_desc = READ_WRITE`, Query Store étant actif par défaut sur les bases utilisateur, ce qui n'est pas le cas sur 2019. Une fixture d'état OFF doit donc le désactiver explicitement sur 2022 ; une fixture qui se contente de créer la base y teste l'état READ_WRITE en croyant tester OFF. Écrire la désactivation sans condition de version, elle est inoffensive sur 2019. ERROR et propriétés inconnues via backend de test, pas corruption de base. Comparer exports SQL/XML aux valeurs UTF-8 retournées via admin ; assertions sur octets complets et non sur seules tailles.
- [ ] Ajouter injections d'erreur déterministes dans les tests de sortie/collecteur : échec à la Nième écriture, manifeste trop long, dépassement pendant le deuxième fichier, lecture SQL interrompue après N lignes, invalid XML après export. Résultat JSON toujours syntaxiquement complet tant que stdout fonctionne. Un pipe stdout fermé peut empêcher toute réponse complète : constater l'échec d'écriture sans prétendre le contraire.

```go
// Writer de panne local au test ; aucun flag de production associé.
type failWriter struct{}
func (failWriter) Write([]byte)(int,error){return 0,errors.New("fixture disk failure")}
```

- [ ] Vert : suite complète sur 2019 puis 2022 ; ne lancer 2025 smoke que pour info/status/top/query/plan/obj table. Enregistrer skip éventuel de Windows comme couverture manquante, pas release validée. `go test -race ./...`, `go vet ./...`.
- [ ] Commit : `test: validate diagnostic workflows permissions and failure contracts`.

### Tâche 16 : documentation, agent skill et builds reproductibles

**Fichiers :** créer `Makefile`, `.github/workflows/ci.yml`, `docs/usage.md`, `docs/permissions.md`, `skills/argosql/SKILL.md` ; modifier README.md et docs/testing.md ; créer `tests/integration/windows_smoke_test.go` si adaptation nécessaire.

**Interfaces :** contrats CLI stables ; aucune API de diagnostic supplémentaire.

- [ ] Écrire un test de cohérence de registre dans `internal/cli/registry_test.go` : chaque commande annoncée a un handler, les flags de la spec sont présents, les commandes différées sont absentes. Ajouter les deux commandes de build aux checks :

```sh
go test ./...
go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/asq-linux-amd64 ./cmd/asq
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -o dist/asq-windows-amd64.exe ./cmd/asq
```

- [ ] Vérifier d'abord l'échec de cohérence si un handler du registre est volontairement absent dans le faux registre de test, puis restaurer le vrai registre et obtenir le vert. Ne pas modifier la production pour fabriquer un échec artificiel.
- [ ] Makefile : test, race, vet, build, integration-2019, integration-2022, smoke-2025. Les cibles intégration fixent ASQ_TEST_IMAGE et utilisent `-tags=integration -count=1 -timeout=15m`. Pas de pull implicite si l'image locale existe ; absence d'image annoncée explicitement. CI Linux exécute unit/race/vet, puis des jobs intégration distincts pour 2019 et 2022 via Podman, timeout de job 30 minutes ; CI Windows exécute unit/build et smoke binaire help/erreurs config sur windows-latest. Utiliser actions/checkout et actions/setup-go avec révisions figées après vérification à l'exécution.
- [ ] Documenter commande Windows contre SQL Server accessible : variable privée ASQ_WINDOWS_TEST_CONFIG et principal limité sur une fixture dédiée. Le test réseau Windows est conditionnel à l'environnement, mais son absence bloque la revendication « connexion Windows testée » ; ne pas remplacer cela par une cross-compilation. Le rapport de release indique exactement la couverture obtenue.
- [ ] README : positionnement, installation des binaires construits, exemple profil true par défaut, workflow, limites. docs/permissions reprend Q/I/S et script de provisioning versionné ; docs/testing donne version YAML résolue, commandes de reproduction, TLS et nettoyage limité au run. Ne publier aucun mot de passe ni clé privée.
- [ ] Utiliser le skill-creator au moment de créer `skills/argosql/SKILL.md`. Contenu minimal : lancer help --json, choisir profil/base, status avant analyse, suivre query->plan->objet, lire les artefacts à la demande, interpréter complétude/permissions. Aucun conseil de forçage, création d'index ou SQL arbitraire.

```markdown
---
name: argosql
description: Diagnose SQL Server Query Store and object metadata using the asq CLI.
---
Start with `asq help --json`. Require an explicit context and database.
Read `qs status` before interpreting historical metrics. Follow query IDs to
plans and referenced objects. Inspect completeness metadata before treating
missing rows as absence. Load large artifacts only when needed.
```

- [ ] Vert : vérification liens locaux et help, tests et builds requis. Rejouer uniquement les suites affectées par un changement de code ; ne pas relancer toute l'intégration pour une correction rédactionnelle seule.
- [ ] Commit : `docs: document supported diagnostics and reproducible validation`.

## Traçabilité spec -> tâches

| Exigence | Tâches |
| --- | --- |
| Toutes les commandes / registre / flags / help offline | 9–14, 16 |
| Config YAML, TLS true par défaut, validation optionnelle, secrets | 2–4, 9, 15 |
| Connexion conservée, lock timeout, deadlines et interruption | 3–4, 9, 15 |
| Permissions Q/I/S, visibilité ambiguë, statistiques partielles | 8–9, 11, 13–15 |
| Fenêtres, moyennes pondérées, internes, groupes de réplique | 10–11, 15 |
| États Query Store et code de résultat selon commande | 9–12, 15 |
| SQLplan fidèle, déclaration XML et parsing borné | 6, 12, 15 |
| TSV/JSON exacts, troncature, plafonds et métadonnées | 1, 5–7, 15 |
| Tailles sans double comptage, DMV par base, preuves d'index | 13–15 |
| Artefacts sécurisés, erreurs et rétention | 6–7, 15–16 |
| Podman isolé, preuves rejouables, 2019/2022/2025 | 4, 8, 10–15 |
| Windows runtime et Linux, packaging et guide agent | 15–16 |

## Validation finale et passage à l'exécution

- [ ] Les 16 tâches et leurs tests sont terminés ; aucun code 0 ne masque une erreur ou une collecte partielle.
- [ ] Le rapport final sépare tests réellement passés, expériences historiques et plateformes non testées.
- [ ] Les artefacts de build sont locaux ; aucune publication/release GitHub automatique.
- [ ] Présenter le diff et le rapport de validation, puis suivre le workflow de fin de branche autorisé par l'utilisateur.

Ce plan est à exécuter par tâches avec le workflow SuperPowers choisi. La rédaction du plan n'a exécuté aucun test applicatif, créé aucune fixture SQL et modifié aucun conteneur.


## Suivi de la review externe

[Review Claude Code intégrale](2026-09-08-argosql-mvp-claude-review.md), reçue après la première rédaction. Corrections appliquées : encodeur push ; décodeur inverse attribué à la tâche 5 ; propriété des métadonnées de collecte/aperçu ; flags et chemins attribués au CLI ; mapping Major=15/16/17 explicite ; types calculés et tolérances ; Lab.Run en sous-processus uniquement ; conservation des fichiers complets ; fixtures Query Store configurées ; clarification execution_type/réplique ; sous-unités de revue 4a/4b et 9a/9b ; CI par image.

Deux suggestions de cette première relecture ne sont pas appliquées telles quelles : retenir N × colonnes × 32 KiB pourrait encore produire un gros tampon, donc la relecture conserve une réserve par table bornée et son décodeur est maintenant planifié ; aucun repli YAML automatique vers une autre API n'est ajouté. La version v4 doit être résolue et figée au début de l'exécution ; en cas d'échec de disponibilité, résoudre cette dépendance explicitement avant de coder Load. La review n'a pas été rejouée après ces corrections.

### Deuxième panel, 8 septembre 2026

Trois lecteurs sur le plan corrigé, au commit 2caf0d3 : agy avec le prompt
directif, agy avec le prompt neutre, un Claude neuf sur le prompt neutre. Kimi
écarté, quota épuisé. Six trouvailles retenues sur dix, appliquées ci-dessus.

1. Le plan fondait ses trois états de permission sur `HAS_PERMS_BY_NAME`, que la
   spec ne nomme nulle part. Mesuré sur 2022 RTM-CU26 : la fonction rend deux
   valeurs, pas trois, et son NULL ne signale qu'une sonde malformée. Tâche 8
   réécrite, avec la table des retours mesurés, le mapping de `Unknown` vers un
   défaut de code, et un test d'intégration qui énumère les sondes réellement
   émises au lieu du test unitaire sur valeurs fabriquées, qui passait sans rien
   vérifier.
2. La sonde niveau instance s'écrit `(NULL, NULL, @permission)`. Avec la classe
   `'SERVER'` elle rend NULL en permanence. `ServerProbe` ajouté.
3. `missing.sql` ne joignait pas les noms locaux, alors que la spec impose des
   LEFT JOIN pour que la visibilité des métadonnées ne retire pas de preuve DMV.
4. La réserve d'octets par table de l'aperçu écartait entièrement une ligne trop
   grosse, donc `--no-truncate` sur une grosse cellule produisait un aperçu vide.
   La ligne est désormais retenue tronquée, avec assertion.
5. Les compteurs d'aperçu vivaient dans la structure que le collecteur sérialise
   avant que Render ne les calcule, donc le manifeste portait `rows_shown=0`.
   `model.Completeness` scindée en `Completeness` et `PreviewState`.
6. Rien ne disait que les blocs SQL du plan sont des esquisses que la prose
   étend. Règle globale ajoutée, et une assertion par tâche concernée.

Quatre trouvailles rejetées, toutes du même moule : le lecteur exécutait un bloc
SQL verbatim, comme le prompt l'exigeait, et signalait des colonnes que la prose
juste après ajoute. Ce moule est devenu la trouvaille 6 plutôt qu'un rejet sec.

Vérifié personnellement plutôt que cru sur parole, parce que ces points portent
la tâche 3 : `db.Conn(connectCtx)` suivi d'un cancel immédiat laisse la connexion
utilisable et `SET LOCK_TIMEOUT 5000` persiste sur la même session ; les noms de
paramètres du DSN sont bien lus par go-mssqldb v1.11.0 et le moteur rapporte
`encrypt_option=TRUE` ; `INTERVAL_LENGTH_MINUTES = 1` est accepté. Les points 1
et 2 ci-dessus ont été mesurés avant d'être écrits, pas déduits du rapport.

### Troisième lecteur, rendu après les six corrections ci-dessus

Le Claude neuf sur le prompt neutre a rendu environ trois fois ce que les deux
agy avaient produit, avec neuf trouvailles vérifiées en exécutant. Toutes
appliquées, plus les trous d'attribution qu'il signalait en lecture.

Trous d'attribution, la catégorie la plus chère parce qu'un implémenteur comble
un trou en inventant et que ses tests passent :

- Le convertisseur des valeurs du pilote vers `model.Cell` n'appartenait à
  aucune tâche et n'était dans aucune carte de fichiers, alors que les quatorze
  diagnostics en dépendent et que `decimal`, `money`, `varbinary` et
  `uniqueidentifier` arrivent tous en `[]uint8`. Attribué à la tâche 5,
  `internal/output/scan.go`.
- `Cell` n'avait pas de `MarshalJSON`, donc se sérialisait en objet plutôt qu'en
  valeur nue positionnelle. Attribué à la tâche 1.
- Aucun flag par commande n'était nommé nulle part. Table complète ajoutée à la
  tâche 9, avec les bornes de la spec, et `Flag` étendu pour porter énumérations
  et exclusions mutuelles qu'il ne pouvait pas exprimer.
- `PreviewOptions` n'atteignait pas le collecteur à qui la tâche 7 confiait la
  construction des candidats, alors que la même tâche interdisait de changer les
  signatures. La responsabilité passe entièrement à `Render`.

Défauts mesurés sur le moteur ou le pilote :

- `ResetSession` ne se déclenche pas à la remise au pool mais à l'acquisition
  suivante ; l'étape de la tâche 3 observait 5000 là où elle annonçait -1.
- `ca_file` : le pilote dispatche sur l'extension, donc un `.crt` valide donnait
  un code 3 à la connexion là où la spec exige 2 avant connexion, et
  `AppendCertsFromPEM` voit sa valeur de retour ignorée par le pilote.
- Un `go test -run` dont le filtre ne matche rien rend `ok` et sort 0. Chaque
  commande de test porte désormais le nombre de tests attendu.
- La couverture de `qs status` annonçait une fenêtre sur une base sans
  historique, un intervalle vide existant dès la création.
- Query Store est actif par défaut sur les bases utilisateur de 2022, ce qui
  rendait muettes les fixtures d'état OFF de la tâche 15.
- `go.yaml.in/yaml/v4` n'existe qu'en `v4.0.0-rc.6` : la décision réelle est de
  figer une pré-version.
- L'enveloppe d'erreur fixe de la tâche 7 ne pouvait pas sortir de
  `model.Result`. Type dédié `model.FallbackError` ajouté à la tâche 1.
- `NormalizeXML` écrit dans un `io.Writer` quand `Sink.File` attend un
  `io.Reader` : la même incompatibilité push/pull que la relecture précédente
  avait corrigée ailleurs sans atteindre cet endroit.

Deux enseignements de méthode, notés parce qu'ils se reproduiront. Le lecteur
directif avait classé le test `ResetSession` de la tâche 3 en « pas un
problème » : sa conclusion était juste et son mécanisme faux, et c'est le
mécanisme que le plan décrivait. Un prompt qui nomme les suspects fait répondre
à une question étroite et bénir ce qui l'entoure. Et le lecteur neutre le plus
fort a trouvé seul plus que les deux autres réunis : dépenser sur un lecteur de
plus avant de dépenser sur de meilleures questions.

Les corrections ci-dessus n'ont pas été relues à leur tour. C'est la même
exposition que celle qui a produit les trouvailles 4 et 5.
