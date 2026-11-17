"""get_poll_results reads the bridge's `polls` and `poll_votes` tables: each
option with its voters, a withdrawn vote counting for nobody."""

import json
import sqlite3

import whatsapp

GROUP = "120363000000000001@g.us"


def test_poll_results(tmp_path, monkeypatch):
    path = tmp_path / "messages.db"
    conn = sqlite3.connect(path)
    conn.executescript("""
        CREATE TABLE polls (id TEXT, chat_jid TEXT, question TEXT, options TEXT, PRIMARY KEY (id, chat_jid));
        CREATE TABLE poll_votes (poll_id TEXT, chat_jid TEXT, voter TEXT, options TEXT, timestamp TIMESTAMP,
                                 PRIMARY KEY (poll_id, chat_jid, voter));
    """)
    conn.execute("INSERT INTO polls VALUES ('p1', ?, 'Cena?', ?)", (GROUP, json.dumps(["Pizza", "Sushi"])))
    for voter, chosen, at in [("391", ["Pizza"], "2026-01-01 10:00"), ("392", ["Pizza", "Sushi"], "2026-01-01 10:01"),
                              ("393", [], "2026-01-01 10:02")]:
        conn.execute("INSERT INTO poll_votes VALUES ('p1', ?, ?, ?, ?)", (GROUP, voter, json.dumps(chosen), at))
    conn.commit()
    conn.close()
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
    monkeypatch.setattr(whatsapp, "get_sender_name", lambda jid: f"name-{jid}")

    got = whatsapp.get_poll_results("p1", GROUP)
    assert got["ok"] and got["question"] == "Cena?" and got["people_voted"] == 2
    counts = {o["option"]: o["votes"] for o in got["options"]}
    assert counts == {"Pizza": 2, "Sushi": 1}
    assert not whatsapp.get_poll_results("nope", GROUP)["ok"]
