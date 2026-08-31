# ChatGPT playlist export prompt

Recommend music matching this request:

> {{YOUR MUSIC REQUEST}}

Return between 6 and 50 tracks, using fewer tracks when the topic is narrow. Prefer
the canonical studio recording unless I explicitly request a live performance,
remix, remaster, edit, clean version, or another variant. Favor releases available
in the highest quality, but do not substitute a different performance merely for
higher resolution.

Return only one valid JSON object—no Markdown fence and no prose—matching this
shape exactly:

```json
{
  "schema_version": "1.0",
  "name": "Playlist title",
  "description": "Brief curatorial description",
  "source_prompt": "The music request above",
  "tracks": [
    {
      "position": 1,
      "title": "Exact recording title",
      "artists": ["Primary artist", "Featured artist if applicable"],
      "album": "Canonical album when known",
      "release_year": 2020,
      "duration_seconds": 240,
      "isrc": "ISRC when confidently known, otherwise omit",
      "version_hints": ["studio", "original album version"]
    }
  ]
}
```

Do not invent ISRCs, durations, albums, or years. Omit optional fields when unsure.
Keep distinct movements and similarly named recordings unambiguous through the
title, artist, album, and version hints.
