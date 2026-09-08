# Relecture croisée de la spec MVP argosql

Note de suivi du 8 septembre 2026 : ce rapport conserve les observations et
propositions historiques. La [spec corrigée](2026-09-08-argosql-mvp-design.md)
porte les contrats applicables, notamment le choix utilisateur de
`TrustServerCertificate=true` par défaut. Les conclusions sur le GROUP BY,
la mémoire du parseur et le plafond de collecte y ont été nuancées. Cette
mise à jour documentaire ne constitue pas une nouvelle exécution des expériences.

Document relu : `2026-09-08-argosql-mvp-design.md`, à son état initial, avant
toute implémentation.

## Comment ce rapport a été produit

Panel prévu à cinq lecteurs (agy directif, agy neutre, Kimi directif, Kimi
neutre, un Claude neuf sur le prompt neutre), plus ma propre lecture.

Trois lecteurs sur cinq ont tourné. Les deux instances Kimi sont mortes au
lancement sur un quota hebdomadaire épuisé (`You've reached your weekly
(7-day) usage limit`), la clé API Moonshot de secours répond `Invalid
Authentication`, et `gemini`, seul autre fournisseur installé, exige une
authentification interactive. Le panel est donc incomplet, et la règle qui
veut qu'on prenne l'union et jamais l'intersection s'applique à un
échantillon plus étroit que prévu. Les deux lectures manquantes restent à
faire quand le quota se rouvre.

Tous les lecteurs disposaient de conteneurs SQL Server 2019 (RTM-CU32-GDR) et
2022 (RTM-CU26) montés pour l'occasion, de Go 1.27 et de
`github.com/microsoft/go-mssqldb v1.11.0`, avec la consigne d'exécuter les
artefacts du document verbatim plutôt que de raisonner de mémoire. J'ai
revérifié moi-même les trouvailles les plus coûteuses, et poussé deux
questions au-delà de là où le panel s'est arrêté.

## Ce qui bloque l'implémentation

### 1. Le modèle de permissions promis n'existe pas, et il en faut trois

Le document promet un principal dédié « granted only permissions required by
the selected diagnostics », cantonné à une base. Les treize commandes se
répartissent en fait sur trois régimes incompatibles.

Le Query Store se contente de `VIEW DATABASE STATE`, cantonné à la base. La
moitié Query Store du MVP tient donc bien dans la promesse.

`idx usage`, `idx missing` et l'heure de démarrage du serveur exigent une
permission d'instance, dont le nom change entre les deux versions
supportées :

    2019 : Msg 300, VIEW SERVER STATE permission was denied
    2022 : Msg 300, VIEW SERVER PERFORMANCE STATE permission was denied
    2019 : GRANT VIEW SERVER PERFORMANCE STATE -> Msg 102, Incorrect syntax near 'VIEW'

Une fois accordée sur 2022, cette permission ouvre `sys.dm_exec_query_stats`
pour toutes les bases de l'instance. Le DBA du client doit donc accepter une
concession d'instance pour obtenir deux commandes sur treize, et le document
ne le dit nulle part.

`obj table`, `obj code`, `size table`, `idx list` et `stats list` exigent
`VIEW DEFINITION`. Sans elle, `OBJECT_ID('dbo.Orders')` rend NULL : l'objet
n'est pas visible, et tous les prédicats en aval sont vides.

    objid|NULL     sys_stats|0     obj_code|0     size_table|NULL|NULL

`stats list` va plus loin : `sys.dm_db_stats_properties` est filtrée par
statistique sur le `SELECT` des colonnes concernées. Avec `VIEW DEFINITION`
mais `DENY SELECT ON SCHEMA::dbo` :

    sys_stats|3     stats_props|0

Après un `GRANT SELECT ON dbo.Orders(id)`, une statistique sur trois
réapparaît ; après un `GRANT SELECT` sur la table entière, les trois, et
`sys.dm_db_stats_histogram` passe de 0 à 171 lignes, c'est-à-dire que les
histogrammes que le document renvoie en deferred deviennent lisibles comme
effet de bord.

Or `docs/TASKS01.md`, dont le document se réclame, recommande exactement
l'inverse : `DENY VIEW ANY DEFINITION`, `DENY VIEW DEFINITION ON SCHEMA::dbo`,
exposition par vues, aucun accès aux tables. Sous cette posture, six commandes
sur treize rendent zéro ligne sans erreur. L'acceptance check 7 échouerait.

À trancher : publier la matrice commande vers permission dans le document, et
assumer que l'inspection d'objets et la confidentialité des données métier
sont en tension, au lieu de laisser croire qu'un seul principal satisfait les
deux.

### 2. Cinq commandes ne peuvent pas distinguer « pas le droit » de « n'existe pas »

Conséquence directe du point précédent, et elle contredit deux règles écrites :
« Permission errors must not be converted to empty results » et le code de
sortie 4 pour « permission or feature unavailable ». Les règles de visibilité
des métadonnées de SQL Server ne lèvent aucune erreur : elles rendent zéro
ligne. Le code 4 est donc inatteignable pour `obj table`, `obj code`,
`size table`, `idx list` et `stats list`, et l'acceptance check 3, qui exige
que les échecs de permission restent distincts des résultats vides, ne couvre
que les scénarios Query Store.

Il faut une sonde de permission explicite dans `internal/sqlserver`, présente
dès la tranche 1, pas un changement de message.

### 3. Le lock timeout de 5 secondes ne survit pas à `database/sql`

Le document nomme trois garanties opérationnelles et une pile qui n'en rend
qu'une. `database/sql` est un pool, `go-mssqldb` implémente
`driver.SessionResetter`, et un `SET` émis par `db.Exec` est annulé avant
l'instruction suivante, sur le même SPID.

C'est le point où j'ai poussé plus loin que le relecteur qui l'a trouvé : la
parade évidente, celle que le document décrit littéralement (« Keep one
connection per invocation »), ne marche pas non plus.

    baseline (no encrypt param)   spid=54 LOCK_TIMEOUT=-1
    after db.Exec SET             spid=54 LOCK_TIMEOUT=-1
    MaxOpenConns(1) after SET     spid=55 LOCK_TIMEOUT=-1
    MaxOpenConns(1) next stmt     spid=55 LOCK_TIMEOUT=-1
    sql.Conn after SET            spid=54 LOCK_TIMEOUT=5000
    sql.Conn second stmt          spid=54 LOCK_TIMEOUT=5000
    sql.Conn reacquired           spid=54 LOCK_TIMEOUT=-1

`-1` est l'attente infinie. Limiter le pool à une connexion ne conserve rien :
le reset a lieu à chaque restitution, quelle que soit la taille du pool. Seul
un `*sql.Conn` tenu ouvert pendant toute l'invocation préserve le réglage, et
il le perd dès sa fermeture. Le document doit donc écrire `sql.Conn`, pas
« une connexion par invocation », et l'acceptance check 6 doit vérifier
`@@LOCK_TIMEOUT` depuis l'intérieur de la session de diagnostic : tel qu'il
est rédigé, il passe avec un timeout infini.

### 4. Le chiffrement n'est pas le défaut du pilote, et le défaut du document n'est jamais testé

Sans paramètre `encrypt`, la connexion réussit en clair, silencieusement :

    baseline (no encrypt param)   encrypted=false

Le chiffrement est donc quelque chose que le binaire doit forcer, pas quelque
chose qu'il hérite, et le mode de défaillance de l'oubli est une connexion en
clair qui marche. Avec le défaut annoncé par le document :

    encrypt=true  FAIL: x509: cannot validate certificate for 127.0.0.1
                  because it doesn't contain any IP SANs

Les conteneurs de la section validation présentent un certificat auto-signé
sans SAN. Tous les acceptance checks passent donc par l'échappatoire de
développement, ce qui fait de la configuration livrée la seule que la suite
d'acceptation n'exerce jamais.

### 5. `qs top --object` ne mène jamais d'une table à ses requêtes

`sys.query_store_query.object_id` porte le module parent, pas les objets
référencés, et vaut 0 pour tout ce qui est ad hoc, y compris la forme
auto-paramétrée :

    8  | 0         | NULL     | INSERT dbo.Orders(...)
    9  | 0         | NULL     | (@1 numeric(2,1))SELECT COUNT(*) FROM [dbo].[Orders]...
    11 | 933578364 | usp_Pick | SELECT COUNT(*) FROM dbo.Orders WHERE cust=7

Un nom de table n'y correspond jamais. Or le workflow d'ouverture du document
prend `dbo.Orders` comme objet d'intérêt : les deux moitiés de l'histoire de
référence ne se rejoignent pas. `TASKS01` restreignait `--object` à
« procédure, fonction ou trigger » ; le document a gardé l'option en perdant
la restriction.

## Défauts de spécification à trancher

1. La formule des totaux contredit la métrique `executions`.
   `sum(avg_metric * count_executions)` n'a pas de sens pour `executions`, où
   le total est `sum(count_executions)`. La phrase voisine montre que
   l'asymétrie a été vue du côté des moyennes et pas du côté des totaux.

2. Grouper par `execution_type` est sans effet une fois le filtre
   `execution_type = 0` posé. Dire dans quel ordre les deux s'appliquent, ou
   énoncer le grain comme (plan, intervalle).

3. Un plan du Query Store contient toujours exactement un statement : trois
   plans pour une procédure de trois instructions, un `<StmtSimple>` chacun.
   « Statement count » vaut donc toujours 1, et le résumé a été spécifié
   contre `sys.dm_exec_query_plan` plutôt que contre `sys.query_store_plan`.
   `internal/plan`, décrit comme un résumé XML en flux à état borné, est
   dimensionné pour un plan de batch de 300 Ko.

4. Query Store OFF ne veut pas dire pas d'historique : en OFF, 145 lignes de
   `sys.query_store_runtime_stats` restent lisibles, sans erreur, et intactes
   après retour en READ_WRITE. Le document ne définit que le cas OFF sans
   historique. Le cas de terrain est OFF avec un historique gelé, et rien ne
   dit ce que `qs top` doit alors faire. L'acceptance check 3 exerce donc un
   cas qu'aucune exigence ne définit.

5. `readonly_reason` vaut 0 quand le store est délibérément READ_ONLY. La
   sortie « raw and decoded read-only reasons » n'a pas de règle de rendu pour
   zéro, c'est-à-dire dans l'état qui compte le plus souvent.

6. Les DMV d'index sont à portée instance et le filtre `database_id = DB_ID()`
   n'apparaît nulle part. `object_id` n'est unique qu'à l'intérieur d'une base,
   et `sys.dm_db_missing_index_group_stats`, qui porte les colonnes de la
   formule, n'a aucune colonne `database_id` : le filtre doit transiter par
   `sys.dm_db_missing_index_groups` vers `_details.database_id`. Le document
   donne la formule et jamais la jointure.

7. Le plafond de 10 000 lignes et le code de sortie 7 sont inatteignables :
   `qs top` est plafonné à 100, `idx missing` à 10, `obj table` aux 1 024
   colonnes du moteur. Soit le plafond appartient à une commande qui n'existe
   pas encore, soit le nombre doit descendre à quelque chose qu'un `idx list`
   sur une table partitionnée peut atteindre.

8. Les budgets stdout ne se hiérarchisent pas. Trois sections à dix lignes
   dépassent 32 KiB avant l'enveloppe JSON. « Stop adding rows before
   exceeding the budget » rend « 10 rows per table » non contraignant pour la
   dernière table sérialisée, et l'ordre de sérialisation n'est nulle part :
   pour un agent, c'est la différence entre « la liste d'index est vide » et
   « la liste d'index a été coupée ». Soit répartir le budget par section,
   soit restaurer le `--preview N` de la source.

9. Le tableau des codes de sortie ne couvre pas les cas de sa propre section
   Query Store : OFF sans historique n'a pas de code désigné, pas plus que le
   décalage plan/requête ni l'identifiant introuvable.

10. Les requêtes internes seront classées comme des requêtes utilisateur.
    `is_internal_query` existe sur les deux versions, une requête StatMan
    apparaît après quarante exécutions d'une charge triviale, et `TASKS01`
    prévoyait `--include-system exclu par défaut`. Le document n'en parle ni
    dans les commandes ni dans les reports.

11. L'acceptance check 5 recopie une limite qui n'existe pas dans cette pile.
    Les « 256 caractères » sont la largeur d'affichage de `sqlcmd`
    (`docs/TASKS01.md:133`), neutralisée par `-y 0 -Y 0`. Avec go-mssqldb, un
    plan de 88 Ko fait l'aller-retour exact. Le check passe par construction,
    et c'est lui qui sera rapporté au vert comme preuve que les gros exports
    sont sûrs.

12. Les trois liens Microsoft utilisent `view=sql-server-ver17`, qui rend la
    page SQL Server 2025, pour une baseline 2019 et 2022.

13. Plancher de version : `sys.query_store_runtime_stats` gagne six colonnes
    en 2022 et `sys.query_store_plan` quatre, dont `replica_group_id`. Sur une
    instance 2022 autonome, `replica_group_id` vaut 1 pour 100 % des lignes et
    `sys.query_store_replicas` est vide. Ajouter la colonne au grain est donc
    sans risque, mais joindre `sys.query_store_replicas` pour nommer la
    réplique ferait disparaître toutes les lignes sur l'instance de référence
    du projet. C'est l'instruction que le document doit porter, et aucun
    lecteur n'était allé jusque-là.

14. Le plan du Query Store ne porte pas de déclaration XML : le texte commence
    à `<ShowPlanXML xmlns=...`. La règle « XML declarations must match the
    encoding » ne régit donc rien pour l'artefact pour lequel elle a été
    écrite, et elle a remplacé l'instruction réelle de la source, qui était de
    retirer l'attribut d'encodage pour ne pas reproduire le problème UTF-16LE
    des `.sqlplan` SSMS. Ce piège suppose une déclaration, et il n'y en a pas
    ici : écrire le XML tel qu'il vient donne un document conforme, la
    déclaration étant facultative et son absence, sans BOM, valant UTF-8.

    Correction apportée après coup, et le défaut vaut d'être noté. J'avais
    ajouté ici une question ouverte, « SSMS ouvre-t-il un `.sqlplan` UTF-8 sans
    déclaration », et une tâche de vérification correspondante dans la spec.
    Elle n'avait pas lieu d'être : je fabriquais une vérification à partir
    d'une règle que je venais moi-même de déclarer sans objet. Elle a survécu
    jusqu'au commit parce que c'était un correctif de relecture, et qu'un
    correctif est moins relu que ce qu'il corrige.

## Couverture de la source

La phrase « Deferred: ... » se présente comme exhaustive. Union des lectures,
capacités de `TASKS01` absentes des deux listes : `idx operational`,
`obj list`, `obj deps`, `obj columns --search`, `size db`, `size top`,
`idx fk-unindexed`, `qs hashes`, `qs adhoc`, `qs variation`, `config drift`,
`qs config-script`, `qs purge-script`, la troncature paramétrable
(`--preview N`, `--truncate N`, `--no-truncate`, `--out`, `--plans-dir`, que
`TASKS01` pose comme une exigence et que le document fige en constantes), le
mode `--expert`, tous les filtres de `qs top` sauf `--object` et
`--min-executions`, `--days`, dix des quatorze métriques de classement, les
compteurs de requêtes, plans et textes distincts de `qs status`, la mémoire et
les cœurs visibles de `info`, et le garde-fou `--min-uptime` avec refus de
répondre.

Chacune tient en une ligne dans la liste des reports. Le coût de ne pas les y
mettre se paie pendant la tranche 3.

## Ce que la relecture a confirmé comme juste

Utile à consigner, parce que ça distingue ce qui a été regardé de ce qui ne
l'a pas été.

- Les unités : une instruction de 31 493 ms au chronomètre est enregistrée
  `avg_duration = 31491772` et `avg_cpu_time = 61899`. Microsecondes et pages
  de 8 Ko, exactement comme le document l'écrit.
- La multiplicité d'intervalle actif est réelle, reproduite deux fois
  indépendamment (3 076 + 699 exécutions sur un même triplet, et deux lignes
  pour quinze exécutions), et la formule pondérée y résiste : sommer les
  lignes ne double compte pas.
- La formule d'index manquant est calculable telle quelle, ses quatre colonnes
  existent bien dans `sys.dm_db_missing_index_group_stats`, et la division par
  100 est correcte puisque `avg_user_impact` est un pourcentage.
- La citation sur `DENY` dit bien ce que le document lui fait dire, exceptions
  comprises.
- `sys.database_query_store_options` est identique entre 2019 CU32-GDR et 2022
  CU26, 22 colonnes de même nom : le plancher de version tient pour
  `qs status`.
- L'inventaire Podman correspond exactement à la machine, y compris la
  collision annoncée sur le port 11433.
- L'encodage TSV est non ambigu : un `\N` littéral se sérialise `\\N`.
- Le piège du double comptage de `size table` est réel : 39 956 lignes
  cumulées pour une table de 20 000.
- Le pilote v1.11.0 ne fuit ni mot de passe ni DSN dans ses erreurs de
  connexion.
- Un module chiffré rend `OBJECT_DEFINITION` NULL alors que la ligne
  `sys.sql_modules` existe : la distinction demandée est possible, mais elle a
  besoin de trois états (chiffré, non visible, absent) et le document n'en
  nomme que deux.

## Une remarque de méthode

Sur la clause « les moyennes ne s'appliquent qu'aux trois premières
métriques », agy neutre a répondu à la question voisine, correcte, et rangé la
clause dans « not a problem ». Le défaut réel est du côté des totaux, une
phrase plus haut, et c'est un autre lecteur qui l'a trouvé. Ne pas trouver est
ordinaire ; certifier sain est pire, et c'est ce que produit une question trop
étroite. Raison de plus pour relancer les deux lectures Kimi manquantes plutôt
que de considérer ce rapport comme complet.
