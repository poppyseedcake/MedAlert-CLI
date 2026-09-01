# Polish terminal interface prototype

This throwaway prototype compares three navigation structures for MedAlert:

- `A` — status-first control center;
- `B` — task-first guided flow;
- `C` — object-first three-pane browser.

Run it with:

```sh
./prototype/tui/run.sh
```

Open `http://127.0.0.1:4173`. Use the bottom switcher, or use the left and right arrow keys while focus is outside the terminal, to change the variant. Inside the terminal, use the up and down arrow keys to move through actions. Use `1` to `5` to open the main areas, `M` to open monitoring, `?` to open help, and `Esc` to return to the start screen.

This code is a prototype. Do not use it as production code.
