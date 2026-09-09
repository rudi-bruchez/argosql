# argosql

CLI Go nommé `asq` qui diagnostique SQL Server via le Query Store et les vues de
catalogue, et rend ses résultats dans un format dimensionné pour qu'un agent IA
les consomme sans noyer son contexte.

Ce fichier porte les règles propres à **ce dépôt**. Elles ont d'abord vécu dans
des prompts de dispatch, répétées huit fois, ce qui est huit occasions de les
écrire différemment. Elles sont ici une fois.

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

## Conteneurs : six ne sont pas à nous

`sql2022`, `sql2025`, `sqltop-test`, `sqltop-test-2019`, `sqlgopace-mssql`, et un
`act-Release-...` préexistent sur cette machine et **n'appartiennent pas à ce
projet**.

`sql2025`, sur le port 11533, **appartient à l'utilisateur et tourne**. Ne pas
s'en servir comme fixture, ne pas le modifier, ne pas l'arrêter.

Les tests d'intégration créent leurs propres conteneurs, étiquetés
`io.argosql.test=<id du run>`, et les retirent eux-mêmes. Ne nettoyer que ce que
le run a créé, en filtrant sur cette étiquette, jamais en balayant par nom.
Vérifier à la fin qu'aucun `asq-test-*` ne survit et que la liste des six est
identique à l'arrivée.

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

## Fichiers qui ne nous appartiennent pas

`docs/TASKS01.md` est une conversation sauvegardée par l'utilisateur. Elle est
suivie par git et peut apparaître modifiée dans `git status` : **ne pas y
toucher, ne pas la commiter, ne pas l'écraser.**

Un implémenteur de tâche ne touche ni à `docs/` ni à `.superpowers/`, à
l'exception de son propre fichier de rapport sous
`.superpowers/sdd/<plan>/`.

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
