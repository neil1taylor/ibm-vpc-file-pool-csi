# Diagrams

The diagrams in the docs are draw.io files saved as `.drawio.svg`. One file is both the picture and the source:

- GitHub and any Markdown viewer show it as an image.
- draw.io (the desktop app, the web app at app.diagrams.net, or the VS Code extension) opens it for editing, because the file holds a copy of the diagram.

## Rules

- **The text explains. The diagram shows.** A diagram has labels on its parts and short labels on its arrows (two to four words). No sentences, no numbered steps, no notes. The explanation stays in the Markdown text next to the diagram.
- **One idea per diagram.** If a diagram needs a legend of more than three entries, split it.
- **Every diagram is next to the text it shows**, with alt text that names what it shows: `![The hook sidecar opens the newest file of the chain](images/vm-instant-snapshots-hook.drawio.svg)`.
- **Names match the doc.** Use the same words for the same parts as the text.
- **When the text changes, change the diagram.** A diagram that disagrees with its text is a defect.

## File names

`<doc-name>-<what-it-shows>.drawio.svg`, in lower case, in this folder. Example: `vm-instant-snapshots-hook.drawio.svg` for `VM-INSTANT-SNAPSHOTS.md`.

## Style

| Part | Fill | Line | Notes |
|---|---|---|---|
| Area: cluster, node, zone, region | none | `#8d8d8d`, dashed | Label at top left, 12 px, `#525252` |
| Group: pod, share, bucket | `#f4f4f4` | `#a8a8a8` | Label at top left, 12 px |
| Component: a container, controller, service | `#ffffff` | `#525252`, or `#0f62fe` (1.5) for the part the diagram is about | Name in bold |
| Optional or added part | `#edf5ff` | `#0f62fe` (1.5), dashed | |
| File | `shape=note`, `#ffffff` | `#8d8d8d`; `#198038` (1.5) for live or healthy | Name in bold; state on a second line |
| Problem, failure, data loss | `#fff1f1` | `#da1e28` (1.5) | Only where the doc describes a failure |
| Arrow | | `#525252`; colour of its target when it helps | Label 11 px, `#525252`, with a background |

- Font: Helvetica, 12 px for parts, 11 px for arrow labels. Courier New for paths.
- Corners: `rounded=1`, `arcSize=8` for components, `arcSize=4` for areas and groups.
- Space: at least 100 px between boxes that an arrow with a label joins. No label may touch a box.
- Flow goes left to right, or top to bottom. Arrows do not cross.
- No icons, no shadows, no gradients, no 3D.
- Width at most 800 px.

## Making or changing a diagram

1. Open the `.drawio.svg` file in draw.io, edit it, and save. draw.io keeps the `.drawio.svg` format.
2. To make one from a script, write the draw.io XML to a temporary `.drawio` file and export it:
   ```bash
   /Applications/draw.io.app/Contents/MacOS/draw.io -x -f svg -e -b 10 -o docs/images/<name>.drawio.svg <name>.drawio
   ```
   `-e` puts the diagram into the SVG, so it stays editable. Do not commit the temporary `.drawio` file.
3. Look at the result as a PNG before you commit (`-f png -s 2`). Check that no label touches a box or another label.
