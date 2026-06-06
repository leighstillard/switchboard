#!/usr/bin/env python3
"""Query switchboard SQLite database. Usage: dbq.py "SQL QUERY" """
import sqlite3, sys, os, json

DB = os.path.expanduser("~/.local/share/switchboard/switchboard.db")
conn = sqlite3.connect(DB)
conn.row_factory = sqlite3.Row

query = sys.argv[1] if len(sys.argv) > 1 else "SELECT 'hello'"
cur = conn.execute(query)
if cur.description is None:
    # Write statement (no result set): commit so changes persist.
    conn.commit()
    print(f"OK: {conn.total_changes} rows affected")
else:
    rows = cur.fetchall()
    if not rows:
        print("(no rows)")
    else:
        for r in rows:
            print(dict(r))
