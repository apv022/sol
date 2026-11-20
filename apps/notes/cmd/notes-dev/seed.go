package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
)

// Seed writes a handful of realistic notes (links, tags, checklists, code, a table and an image)
// under root/Notes.
func Seed(root string) error {
	files := map[string]string{
		"Notes/Welcome.md":             "# Welcome to Notes\n\nNotes are plain Markdown files in your synced folder, so any editor works and **Files** syncs them.\n\n- Link notes with [[Groceries]] or [[Projects/Sol launch]]\n- Tag them with #welcome #how-to\n- Check things off in the preview: see [[Groceries]]\n\n![A sunset](attachments/dusk.png)\n\nTip: type `[[` to link to another note.\n",
		"Notes/Groceries.md":           "# Groceries\n\n#home #shopping\n\n- [x] Oat milk\n- [ ] Coffee beans\n- [ ] Lemons\n  - [ ] for the cake\n- [ ] Rice\n\nBack to [[Welcome]].\n",
		"Notes/Meeting notes.md":       "# Meeting notes\n\n#work\n\n## Standup\n\n> Ship the notes app before the weekend.\n\n| Who | Doing | Blocked |\n|---|:-:|---|\n| Ada | Search index | no |\n| Bo | Editor | `yes` |\n\nFollow-ups go to [[Projects/Sol launch]].\n",
		"Notes/Projects/Sol launch.md": "# Sol launch\n\n#work #sol\n\nPlan for the launch.\n\n1. Finish the apps\n2. Write the docs\n3. Tell people\n\n```go\nfunc main() {\n\tfmt.Println(\"hello, <world>\")\n}\n```\n\nSee also [[Meeting notes]] and [[Ideas]].\n",
		"Notes/Projects/Ideas.md":      "# Ideas\n\n#sol #ideas\n\n- [ ] Widgets for the home screen\n- [ ] Dark mode for the *printed* page\n- [ ] A note that links to [[Nothing yet]]\n",
		"Notes/Recipes/Lemon cake.md":  "# Lemon cake\n\n#home #baking\n\nIngredients: 200 g flour, 3 eggs, 2 lemons.\n\n- [ ] Preheat to 180 C\n- [ ] Mix\n- [ ] Bake 35 minutes\n",
	}
	for p, s := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(s), 0o644); err != nil {
			return err
		}
	}
	img := image.NewRGBA(image.Rect(0, 0, 320, 160))
	for y := 0; y < 160; y++ {
		for x := 0; x < 320; x++ {
			t := float64(y) / 159
			img.Set(x, y, color.RGBA{uint8(40 + 200*t), uint8(55 + 110*t), uint8(107 - 80*t), 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	d := filepath.Join(root, "Notes", "attachments")
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(d, "dusk.png"), buf.Bytes(), 0o644)
}
