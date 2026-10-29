# results

Runs from 2026-10-29. A free name was free when checked; someone can take it
since. `checked.txt` lines are `name<TAB>verdict`.

| folder | checked | free | notes |
|---|---|---|---|
| `2-char/` | 1369 (`a-z0-9_`) | 0 | 1360 taken, 9 reserved |
| `3-char/` | 50653 (`a-z0-9_`) | 8803 | every 3-letter and 3-digit name taken; free ones mix letters, digits, `_` |
| `faces/` | 385 (+260 in `words/`) | 184 | every 3-char face (`x_x`, `o_o`, `owo`, ...) taken; free ones are 4+ chars |
| `words/` | 123756 | 28586 | no common 3-5 letter word free |

`words/`:

- `words-3-5.txt`: every free dictionary word of 3-5 letters, shortest then most used, with word class and meaning
- `nouns-3-5.txt`, `nouns-4-5-ranked.txt`: nouns only, no verbs, plurals or names
- `ranked-all-lengths.txt`, `ranked-by-length.txt`: every free word of any length, scored like a domain (common, short, base form, noun)
- `input/`: the lists that were checked (popular words, English dictionary, WordNet nouns, dwyl + ENABLE + NLTK + wordfreq + WordNet merged)

`3-char/ranked-leet.txt` ranks the mixed names by how well they read as a word (`5et` = set).
