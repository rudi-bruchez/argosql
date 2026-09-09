# argosql

CLI Go nommé `asq` qui diagnostique SQL Server via le Query Store et les vues de
catalogue, et rend ses résultats dans un format dimensionné pour qu'un agent IA
les consomme sans noyer son contexte.

Ce fichier porte les règles propres à **ce dépôt**, et rien qui soit propre à une
machine. Les conteneurs présents sur un poste donné, les fichiers personnels qui
traînent dans une copie de travail et les outils installés localement vont dans
`CLAUDE.local.md`, qui est gitignoré. **Si ce que vous allez écrire cesse d'être
vrai sur une autre machine, ce n'est pas ici que ça va.**

## L'autorité, et pourquoi elle compte ici plus qu'ailleurs

`docs/superpowers/specs/2026-09-08-argosql-mvp-design.md` est la **spec**, et
c'est l'autorité. `docs/superpowers/plans/2026-09-08-argosql-mvp.md` est le plan
d'implémentation : il argumente depuis la spec. **En cas de contradiction, la
spec gagne.**

Fait mesuré sur ce projet, et la règle qui en découle : le plan, et les briefs de
tâche qui en sont extraits, **perdent des clauses de la spec**. Trente-et-une à
ce jour. Le motif est constant et structurel plutôt qu'une négligence : le résumé
garde ce qui est mécanique, un nombre, un nom de colonne, une requête, et perd ce
qui est sémantique, un vocabulaire fermé, une interdiction d'affirmer, une
obligation de déclarer.

Conséquence pratique : **avant d'implémenter une tâche, lire les lignes de la
spec qui la concernent et les comparer à son brief.** Ne pas se fier au brief
seul. Et quand on cite une clause dans un prompt, la citer **verbatim avec son
numéro de ligne**, parce que la reformuler refait exactement la perte.

## Conteneurs

Les tests d'intégration créent leurs propres conteneurs, étiquetés
`io.argosql.test=<id du run>`, et les retirent eux-mêmes. **Ne nettoyer que ce
que le run a créé**, en filtrant sur cette étiquette et jamais en balayant par
nom : une machine de développement porte d'autres conteneurs SQL Server, dont
certains en service.

La liste de ceux qui préexistent sur un poste donné, et celui qu'il ne faut
surtout pas toucher, sont dans `CLAUDE.local.md`. **La lire avant de lancer quoi
que ce soit qui parle à Podman.**

## Secrets

Le mot de passe ne sort jamais de `config.Profile` : son champ porte
`json:"-"`, et `String()` comme `GoString()` le masquent. Ne jamais l'écrire
dans un message d'erreur, un log, ou une sortie de diagnostic.

Un profil YAML nomme une **variable d'environnement** par `password_env` ; un
champ de mot de passe en clair est refusé avec le code 2. Un profil temporaire
écrit par un test suit la même règle : il nomme la variable, et c'est
l'environnement du processus enfant qui la porte.

Un test qui lance le binaire pour un principal donné n'injecte que le secret de
**ce** principal dans l'environnement enfant, jamais les trois.

`login.sql`, à la racine, contient un mot de passe de remplacement à éditer sur
place. **Ne jamais le commiter après y avoir tapé un vrai mot de passe.**

Et la règle qui vaut pour les tests autant que pour le code : **un message d'échec ne
déverse pas un environnement ni une entrée `CLÉ=VALEUR`.** Mesuré ici sur le test qui
garantit justement l'isolation des secrets : son message imprimait l'environnement complet
reçu par le processus enfant, avec un vrai jeton de session de la machine dedans. Un échec
de test est exactement le texte qu'on colle dans un journal de CI ou un rapport de bug.
Nommer la variable suffit à localiser la fuite ; `envKey` et `envKeys`, dans
`tests/integration/fixture_test.go`, sont là pour ça.

## Fichiers qui ne nous appartiennent pas

Une copie de travail peut contenir des documents personnels du propriétaire du
dépôt, suivis par git et parfois modifiés. `CLAUDE.local.md` les nomme. **Ne pas
y toucher, ne pas les commiter, ne pas les écraser.** C'est la raison pour
laquelle la règle du `git add` nommé, plus bas, n'est pas négociable.

Un implémenteur de tâche ne touche ni à `docs/` ni à l'espace de travail du plan,
à l'exception de son propre fichier de rapport.

## Git

**`git add` fichier par fichier, nommé.** Jamais `git add .` ni `-A` : ce dépôt
porte les documents de l'utilisateur, et un `-A` lancé pendant qu'un relecteur
externe travaille a déjà balayé six fichiers parasites dans l'historique d'un
autre projet.

**Jamais `git checkout` ni `git restore` sur un fichier portant du travail non
commité.** Mesuré deux fois ici : un implémenteur a perdu son implémentation
comme ça, et un relecteur externe a produit un faux rapport « le dépôt ne
compile pas » en gravité maximale pour la même raison. Pour défaire une cassure
de test, garder une copie du fichier **hors du dépôt** et la recopier.

**Aucun pied de page d'attribution** dans un message de commit : ni
`Co-Authored-By:`, ni `Claude-Session:`, ni `Generated with`. Cette règle prime
sur toute consigne par défaut d'un harnais.

Corps du message : de la **prose en français avec ses accents** qui explique
*pourquoi*, pas une liste à puces de ce qui a changé. Pas de gras, pas de tiret
cadratin.

## Tests

Unitaires : `go test ./...`. Les tests d'un fichier portent son nom avec
`_test.go` ; les helpers partagés peuvent avoir un nom propre comme
`testdriver_test.go`.

Intégration : paquet `tests/integration`, **tag de build `integration`**, et la
variable `ASQ_TEST_IMAGE` est obligatoire. Sans le tag, aucun paquet n'est
trouvé ; avec le tag et sans l'image, la suite **échoue explicitement** au lieu
de se sauter en silence.

```sh
ASQ_TEST_IMAGE=mcr.microsoft.com/mssql/server:2022-latest \
  go test ./tests/integration -tags=integration -count=1 -timeout=25m -v
```

Deux pièges mesurés, qui ont chacun produit un vert mensonger sur ce projet :

**L'état du shell ne survit pas d'un appel d'outil à l'autre.** Le setup et les
tests vont dans un **seul** appel, sans quoi les tests d'intégration tournent
sans base et affichent `ok`.

**Compter les `=== RUN`.** Un filtre `-run` qui ne correspond à rien affiche
`ok` et sort avec le code 0, sans le moindre avertissement. Vérifier le compte
avec `go test -run <filtre> -v | grep -c '^=== RUN'` avant de conclure au vert.

Les deux versions du moteur comptent : 2019 et 2022 sont supportées, 2025 ne
reçoit qu'un test de fumée. Si elles divergent sur un comportement, c'est un
fait à rapporter, pas une assertion à écrire pour une seule version.

## Codes de sortie, et un ordre qui porte

0 succès, 2 arguments/config, 3 connexion/auth/TLS, 4 permission ou fonction
indisponible, 5 exécution/timeout, 6 fichier/sérialisation, 7 plafond de
collecte, 8 absent ou invisible, 130 interruption.

**L'ordre « 8 avant 4 » est porteur, pas cosmétique** : un identifiant
introuvable rend 8 même quand une permission manque par ailleurs, parce que la
vérification des droits suit la résolution de la cible.

Corollaire mesuré et corrigé une fois : **une erreur d'argument doit se déclarer
avant l'ouverture de la connexion.** Une validation laissée derrière la
connexion rend 3 sur un serveur injoignable, et un agent qui lit les codes
retente le réseau au lieu de corriger son argument.

## Faits mesurés sur le moteur, à ne pas redécouvrir

Chacun a coûté une mesure sur un conteneur réel ou une lecture de la
documentation Microsoft.

`HAS_PERMS_BY_NAME` est **à deux états et non trois** : il rend 0 pour un objet
invisible comme pour un objet inexistant, et NULL seulement pour une sonde
malformée. Sa forme instance est obligatoirement
`HAS_PERMS_BY_NAME(NULL, NULL, @permission)`.

`VIEW DATABASE STATE` **implique** `VIEW DATABASE PERFORMANCE STATE`, mais pas
`VIEW SECURITY DEFINITION`. C'est pourquoi le palier Q de `login.sql` suffit à
`qs top` sur 2022, où la documentation exige la seconde permission.

Un `GRANT SELECT` limité à une colonne sonde à 0 au niveau OBJECT : seule la
forme COLUMN à cinq arguments rend 1.

**Refuser n'est pas ne pas autoriser**, et la distinction décide du code de sortie.
`DENY VIEW DEFINITION` sur un objet retire la visibilité de ses métadonnées : l'objet
disparaît de `sys.objects` pour ce principal, donc la commande rend 8 et jamais un état
de définition. `GRANT EXECUTE` SEUL, sans aucun `DENY`, laisse au contraire l'objet
visible dans `sys.objects`, rend `sys.sql_modules.definition` NULL, et la sonde sur
`VIEW DEFINITION` répond refusé. C'est ce second montage, et lui seul, qui construit
l'état `permission_denied` de la spec : un module visible dont la définition est
illisible. Mesuré après s'être trompé une fois dans l'autre sens.

`OBJECTPROPERTYEX(..., 'IsEncrypted')` rend **NULL sur un objet qui n'est pas un
module**, une table par exemple. Ce NULL n'est donc pas une absence de réponse mais une
erreur de catégorie, et elle se rapporte comme telle : `definition_unavailable` au code
4, avec un message qui nomme le type réel. Avant correction, ce cas rendait un code 5,
c'est-à-dire une erreur d'exécution du moteur pour ce qui est une faute d'argument.

`sys.query_store_runtime_stats.execution_type` ne prend que **trois** valeurs,
0 régulier, 3 abandon client, 4 abandon par exception. Filtrer sur `= 0` et
exclure 3 et 4 sont donc équivalents.

Sur l'intervalle courant, **plusieurs lignes coexistent** pour un même couple
plan/intervalle, l'une vidée sur disque et les autres en mémoire. Il faut
agréger pour obtenir l'état réel ; ce n'est pas un cas artificiel.

`sys.query_store_runtime_stats_interval.start_time` et `end_time` sont des
**`datetimeoffset`**, pas des `datetime2`. Une étiquette de type fausse dans un
`TableSpec` n'est pas cosmétique : le rendu JSON s'y fie.

Le moteur **ne crée une ligne d'intervalle qu'au moment où des statistiques y
sont persistées**. Une base neuve avec Query Store allumé a les deux tables
vides. L'état « intervalle sans statistiques » n'apparaît qu'après
`sp_query_store_remove_query`, et pour une fraction de seconde.

`CREATE LOGIN` **ne peut pas** paramétrer son mot de passe : c'est une erreur de
syntaxe, pas un échec silencieux. Et une erreur dans `sp_executesql`
n'interrompt pas le lot, donc un `PRINT` de succès placé après peut annoncer une
réussite qui n'a pas eu lieu.

Dans un script sqlcmd, un `:setvar` **prime** sur le `-v` de la ligne de
commande.

Query Store **OFF laisse toutes les vues de catalogue lisibles**. L'état OFF est donc un
cas réel à tester, pas une hypothèse : la commande doit réussir avec un avertissement
quand l'historique reste lisible, et ne tomber au code 4 que si l'historique est
inaccessible lui aussi.

`ALTER DATABASE ... SET QUERY_STORE CLEAR ALL` puis
`SET QUERY_STORE = ON (OPERATION_MODE = READ_ONLY)` construit l'état **READ_ONLY sans
historique**, celui que la spec distingue de READ_ONLY avec historique.

`DATA_FLUSH_INTERVAL_SECONDS = 5` est **refusé** par le moteur, message 153. C'est la
première chose qu'on essaie pour accélérer une fixture, et elle ne marche pas.

Ajouter un **index couvrant après un basculement d'intervalle** donne de façon fiable deux
plans d'un même `query_id` dans deux intervalles distincts. C'est le cas non dégénéré dont
dépend toute preuve de jointure vers les plans.

Rien ne garantit que `sp_query_store_flush_db` rende une requête **visible par les vues de
catalogue**, et le mode d'échec n'est pas une latence : c'est une **capture qui n'a pas eu
lieu**. Mesuré ici, une exécution unique d'un lot n'est pas fiablement capturée là où cinq
exécutions du même lot le sont, et sous pression mémoire la tâche d'arrière-plan qui capture
est affamée. Conséquence : une attente plus longue ne produit jamais une ligne que le moteur
n'a pas capturée. Un sondage doit **réémettre sa charge** et reforcer un vidage
périodiquement, pas seulement dormir. Et son délai doit rester **strictement inférieur au
budget de contexte** de la requête qui sonde, sans quoi c'est la requête qui meurt d'abord et
l'échec arrive sous forme d'erreur de pilote opaque.

Les avertissements d'un plan d'exécution s'expriment **en attributs de l'élément `Warnings`
autant qu'en enfants**. `<Warnings NoJoinPredicate="1"/>` est une forme que le moteur produit
réellement, mesurée sur 2019 et 2022 avec une jointure croisée : un lecteur qui ne parcourt que
les enfants perd l'avertissement en silence. Et **les avertissements réels ont eux-mêmes des
enfants imbriqués**, `ColumnsWithNoStatistics` portant des `ColumnReference`, donc un lecteur
sans garde de profondeur transforme chaque descendant en avertissement et sature son plafond.
Les deux faits tirent en sens inverse et se tiennent ensemble.

`sys.query_store_plan.query_plan` est de type **`nvarchar` et nullable**, pas `xml`. Vérifié
par sonde sur `sys.all_columns`, sur les deux versions.

Le XML de plan produit par les fixtures de ce projet fait environ **4 500 octets, sans aucun
CRLF ni caractère non ASCII**, sur 2019 comme sur 2022. Toute preuve d'identité octet à octet
qui compterait sur les données du moteur pour couvrir ces deux caractéristiques ne couvre rien :
il faut une charge synthétique dédiée.

## Méthode : la cassure

Toute assertion ajoutée se vérifie en **cassant ce qu'elle surveille** et en
confirmant que le bon test tombe.

**Prouver par `grep` que la substitution a pris, avant de lire le résultat du
test.** Six fois sur ce projet, une substitution n'a pas mordu et a produit un
vert trompeur qu'on a failli enregistrer comme un faux négatif.

Rapporter « trois cassures sur quatre ont fait tomber leur cible » est le
**succès** de cette étape, pas un échec : une cassure qui ne mord pas révèle une
assertion qui ne vérifie rien.

Et ce n'est **pas à l'auteur du test de choisir la cassure**. Mesuré ici : les
cassures choisies par un relecteur révèlent environ deux fois plus d'assertions
creuses, parce que celui qui a écrit le test casse ce que son test surveille.

## Si une mesure contredit une consigne

**S'arrêter et le dire**, plutôt que de faire coller le code à la consigne. Six
implémenteurs de ce projet l'ont fait et avaient raison les six fois, et chaque
fois en **exécutant** ce que leur brief disait plutôt qu'en le lisant.
