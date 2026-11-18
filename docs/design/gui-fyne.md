# IHM de bureau Fyne — réplique de l'explorateur web

Statut : proposition · Branche : `gui`

## 1. Objectif

Fournir une application de bureau native (`llm-memory-gui`) offrant **la parité
fonctionnelle** avec l'explorateur web servi par `llm-memory ui`
(`internal/ui/ui.go` + `internal/ui/static/index.html`), sans dégrader le
binaire principal (pur Go, `CGO_ENABLED=0`, cross-compilé pour 4 plateformes).

Hors périmètre de la v1 : fonctionnalités absentes du web (import/export,
réindexation sémantique, handoff…). Elles pourront venir ensuite.

## 2. Inventaire fonctionnel à reproduire

| Zone web | Fonction | Appel store actuel |
|---|---|---|
| En-tête | `+ New`, `Run GC`, stats (`N active · M archived · schema vX`) | `Create`, `ArchiveExpired`, `Stats` |
| Colonne gauche | Liste des namespaces + compteur actif, entrée « All namespaces » | `ListNamespaces` |
| Panneau namespace | Description, renommer, fusionner, archiver tout (sauf `global`) | `EnsureNamespace`, `RenameNamespace`, `MergeNamespace`, `ArchiveNamespace` |
| Barre de filtres | Recherche FTS/sémantique (debounce 200 ms), tags, kind, actif/archivé/tous | `Search` (Engine ou Store), `List` |
| Liste | Cartes : `#id`, kind, namespace, importance, archivé, TTL, titre, aperçu ; pagination 50 | `List` (limit/offset) |
| Détail | Méta, titre, tags cliquables (→ filtre), contenu | `Get` |
| Détail — édition | namespace, kind, importance, titre, tags, nouveau TTL, effacer TTL, contenu | `Update` |
| Détail — actions | Archiver / Restaurer, Find Similar → Supersede | `Archive`, `Restore`, `FindSimilar` (+ fusion sémantique), `Supersede` |
| Liens | Liste entrants/sortants, saut vers #id, suppression, ajout (relates_to / depends_on / supersedes) | `Links`, `AddLink`, `RemoveLink` |
| Historique | Snapshots, bascule diff ligne à ligne / brut | `History` + LCS côté client |
| Bandeau | Message succès (auto-masqué 4 s) / erreur (persistant) | — |

## 3. Décisions d'architecture

### 3.1 Module Go séparé : `gui/`

Fyne exige CGO (GLFW/OpenGL). Pour ne pas toucher au binaire principal ni à la CI
existante (`go vet ./...`, `go test -race ./...` sur 3 OS sans en-têtes GL) :

```
gui/
  go.mod                 module github.com/Morditux/llm-memory/gui
                         require github.com/Morditux/llm-memory v0.0.0
                         replace github.com/Morditux/llm-memory => ../
  cmd/llm-memory-gui/    main.go
  internal/core/         état + contrôleur, SANS import fyne (testable partout)
  internal/view/         widgets Fyne
  Icon.png, FyneApp.toml
```

- Un module imbriqué est exclu automatiquement de `./...` à la racine : aucun
  impact sur `make`, CI et release actuels ; `go.mod` racine reste sans dépendance Fyne.
- La règle `internal/` de Go repose sur le chemin d'import : `…/llm-memory/gui`
  peut importer `…/llm-memory/internal/store`.
- Alternative écartée : build tag `gui` dans le module racine (pollue `go.mod`,
  `go vet ./...` devient fragile).

### 3.2 Accès aux données : in-process, pas via HTTP

L'IHM ouvre directement la base SQLite comme le fait `cmdUI` (WAL +
`busy_timeout(5000)` déjà configurés dans `internal/store/db.go`, donc
cohabitation avec le serveur MCP en cours d'exécution). Pas de serveur HTTP, pas
de CSRF, pas de port.

Alternative écartée en v1 : client HTTP vers `llm-memory ui` (utile pour une
base distante ; pourra être ajouté derrière une interface `Backend`, cf. 3.3).

### 3.3 Extraction d'une couche service partagée (pré-requis)

Aujourd'hui la logique métier de l'UI est mêlée aux handlers HTTP. À extraire
**dans le module racine**, sans changement de comportement :

1. `internal/explorer` (nouveau) — API Go consommée par le web **et** la GUI :
   - `ParseExpiration(in, at string) (*time.Time, error)` (déplacé de `ui.go`)
   - `CreateRequest` → `store.NewMemory` (construction des `LinkRef` related/depends)
   - `Similar(ctx, id)` : FTS + fusion sémantique + filtrage de l'id courant (`mergeSimilar`)
   - `Detail(ctx, id)` : memory + history + links
   - constante `Actor = "ui"` (ou `"gui"`, cf. §7)
2. `internal/bootstrap` (nouveau) — `OpenStore(ctx, flags)` et `NewEngine(...)`
   sortis de `cmd/llm-memory` pour que la GUI réutilise exactement la même
   résolution de config (`config.Register`, chemin DB, embeddings).
3. `internal/ui/ui.go` devient un adaptateur HTTP fin sur `explorer` ; les tests
   existants (`ui_test.go`, `main_test.go`) doivent rester verts sans modification.
4. `LineDiff(old, new string) []DiffLine` porté du JS en Go dans `explorer`
   (LCS identique, même garde `m*n > 250000` → tout supprimé/tout ajouté).

Côté GUI, `core` dépend d'une interface :

```go
type Backend interface {
    Stats(ctx) (store.Stats, error)
    Namespaces(ctx) ([]store.Namespace, error)
    List(ctx, store.ListFilter) ([]store.Memory, error)
    Search(ctx, store.SearchQuery) ([]store.SearchResult, error)
    Detail(ctx, id) (explorer.Detail, error)
    Create / Update / Archive / Restore / Supersede / AddLink / RemoveLink
    SetNamespaceDescription / RenameNamespace / MergeNamespace / ArchiveNamespace
    GC(ctx) (int64, error)
}
```

implémentée par `explorer` (et plus tard, éventuellement, un client HTTP).

### 3.4 Modèle de concurrence Fyne

- Fyne ≥ 2.6 : toute mutation de widget hors du thread UI passe par `fyne.Do`.
- Chaque appel `Backend` s'exécute dans une goroutine avec `context` annulable ;
  le résultat est appliqué via `fyne.Do`.
- Requêtes de liste/recherche : jeton de génération (compteur) pour ignorer les
  réponses obsolètes quand l'utilisateur tape vite (le web a ce défaut, ne pas le copier).
- Debounce 200 ms sur recherche et tags via `time.AfterFunc` réinitialisé.

## 4. Correspondance écran web → Fyne

```
┌ Toolbar: [+ New] [Run GC] ─────────────── stats label ┐
├──────────┬───────────────────────────┬────────────────┤
│Namespaces│ bandeau statut            │ Détail         │
│widget.   │ filtres: Entry Entry      │ (Scroll)       │
│List      │   Select(kind) Select(arch)│               │
│──────────│ widget.List de cartes     │                │
│Panneau ns│ pager ← Page n →          │                │
└──────────┴───────────────────────────┴────────────────┘
 container.NewHSplit(gauche, NewHSplit(centre, droite)), offsets 0.18 / 0.55
```

| Élément web | Fyne |
|---|---|
| Liste namespaces + badge | `widget.List` ; item = `Label` + compteur aligné droite ; sélection = filtre |
| Panneau namespace | `widget.Form` (Entry description + bouton) ; rename/merge : `Entry` + `Button` ; merge-target en `widget.SelectEntry` alimenté par les namespaces existants |
| Archiver namespace | `dialog.NewConfirm` (**ajout** : le web n'a pas de confirmation) |
| Filtres | `widget.Entry` (placeholder), `widget.Select` kind (6 valeurs + « All kinds »), `widget.Select` archived |
| Cartes mémoire | `widget.List` à hauteur variable (`SetItemHeight`) ; ligne méta en `RichText` ou `Label`s colorés par importance (thème) ; aperçu tronqué 3 lignes |
| Pager | 2 boutons + label, masqué en mode recherche |
| New | `dialog.NewCustomConfirm` contenant le formulaire (namespace pré-rempli avec le ns courant ou `global`, kind, importance par défaut 2, titre, tags, TTL, contenu multiligne) |
| Détail lecture | `Label` (wrapping) / `RichText` ; tags = `widget.Button` bas-contraste → applique le filtre tag |
| Détail édition | bascule de conteneur (lecture ↔ formulaire) ; checkbox « Clear expiration » si `expires_at` |
| Similar | section repliable, cartes avec `% match` et bouton « Supersede #id » (confirm) |
| Liens | lignes : badge kind/`←` direction, hyperlien `#id` (saut), bouton `×` si sortant et ≠ supersedes ; ligne d'ajout `Select` + `Entry` numérique + bouton |
| Historique | `widget.Accordion` par snapshot ; diff rendu en `widget.TextGrid` avec styles fond vert/rouge (monospace), ou texte brut ; bouton bascule global |
| Bandeau | barre en haut du centre (`canvas.Rectangle` + `Label`), auto-hide 4 s pour succès |
| Thème | thème sombre personnalisé reprenant la palette CSS (`--bg #0f172a`, `--panel #1e293b`, `--accent #38bdf8`, `--green`, `--amber`, `--red`) ; respecter aussi le mode clair système en option |

Raccourcis clavier (bonus natif) : `Ctrl+N` nouvelle mémoire, `Ctrl+F` focus
recherche, `Ctrl+S` enregistrer en édition, `Échap` annuler l'édition, `F5` rafraîchir.

## 5. Plan de développement par phases

Chaque phase = une PR vers `main`, CI verte vérifiée avec `gh run watch`.

### Phase 0 — Refactor partagé (module racine, sans Fyne) · ~1–2 j
- Créer `internal/explorer` et `internal/bootstrap` (§3.3), y déplacer la logique.
- Faire de `internal/ui` un adaptateur ; aucune modification d'API JSON.
- Porter `LineDiff` en Go + tests table (vide, identique, ajout, suppression, garde 250 000).
- Tests unitaires `explorer` (ParseExpiration, Similar avec/sans moteur sémantique, Create avec liens).
- ✅ Critère : `make all` vert, `ui_test.go` inchangé et vert.

### Phase 1 — Squelette GUI · ~1 j
- Module `gui/`, `fyne.io/fyne/v2` (dernière 2.x stable), `cmd/llm-memory-gui`
  acceptant les mêmes flags de config que `llm-memory ui` (via `config.Register`).
- Fenêtre 1400×850, layout 3 colonnes, thème, icône (convertir `assets/logo.jpg` en PNG).
- Barre d'outils + stats chargées au démarrage.
- `core.Controller` + faux `Backend` en mémoire pour les tests.
- ✅ Critère : la fenêtre s'ouvre sur une vraie base et affiche les stats.

### Phase 2 — Navigation en lecture · ~2 j
- Liste des namespaces, filtres, recherche debouncée, pagination.
- Liste de cartes, sélection → panneau détail (méta, tags cliquables, contenu).
- Liens (lecture + saut), historique avec diff/brut.
- ✅ Critère : tous les parcours de lecture du web reproduits.

### Phase 3 — Mutations mémoire · ~2 j
- Création (dialog), édition, archive/restore, GC.
- Find Similar + Supersede, ajout/suppression de liens.
- Bandeau succès/erreur ; rafraîchissement coordonné (stats + namespaces + liste + détail)
  équivalent à `refresh(id)` du JS.
- ✅ Critère : chaque action produit la même entrée d'historique qu'en web.

### Phase 4 — Gestion des namespaces · ~1 j
- Description, renommage, fusion, archivage (avec confirmation), `global` protégé.
- Après rename/merge, la sélection suit le nouveau nom (comportement web).

### Phase 5 — Finitions · ~1–2 j
- Raccourcis clavier, mémorisation taille fenêtre / splits / dernier namespace (`fyne.Preferences`).
- Rafraîchissement périodique optionnel (le MCP écrit en parallèle) : bouton + timer 30 s désactivable.
- Gestion d'erreurs : DB verrouillée, moteur d'embeddings injoignable → repli FTS silencieux + indication.
- Accessibilité : tailles de police du thème, focus clavier dans les listes.

### Phase 6 — Packaging, CI, docs · ~1–2 j
- Job CI `gui` (matrice ubuntu/macos/windows) : `cd gui && go vet ./... && go test ./...` ;
  sur Ubuntu installer `libgl1-mesa-dev xorg-dev`.
- Release : binaires `llm-memory-gui` via `fyne package` (ou `fyne-cross` pour
  linux/arm64), artefacts `.tar.xz` / `.app` / `.exe` ; ne pas modifier la cible
  `make release` existante, ajouter `make gui` / `make gui-release`.
- `Makefile` : cibles `gui`, `gui-test`.
- README : section « Desktop GUI », capture d'écran, badge Fyne.
- ✅ Critère : workflow release produit les 3 OS, CI vérifiée via `gh`.

Estimation totale : ~9–12 jours.

## 6. Stratégie de tests

| Couche | Outil | Contenu |
|---|---|---|
| `internal/explorer` (racine) | `go test -race` | logique métier, diff, expiration, similar |
| `gui/internal/core` | `go test`, faux Backend | transitions d'état : filtre → requête, pagination, génération obsolète ignorée, refresh après mutation, sélection après rename |
| `gui/internal/view` | `fyne.io/fyne/v2/test` (driver headless) | `test.NewApp`, `test.Type`, `test.Tap` : saisie recherche → liste, clic tag → filtre, édition → `Update` appelé avec le bon `Patch` |
| Intégration | store SQLite temporaire (`t.TempDir()`) | parcours création → édition → historique |
| Parité | checklist manuelle du §2 sur une base de démo (`examples/`) | avant chaque release |

## 7. Questions ouvertes

1. **Acteur** des modifications : réutiliser `"ui"` ou introduire `"gui"` (meilleure traçabilité
   dans `updated_by` / historique) ? Recommandation : `"gui"`.
2. Faut-il un mode **client HTTP** (piloter une instance `llm-memory ui` distante) dès la v1 ?
   Recommandation : non, garder l'interface `Backend` pour l'ajouter plus tard.
3. Distribution : binaire séparé `llm-memory-gui` (recommandé) ou sous-commande
   `llm-memory gui` dans un binaire CGO distinct ?
4. Confirmations supplémentaires (archive mémoire, supersede) non présentes dans le web :
   les ajouter aussi côté web pour garder la parité ?
