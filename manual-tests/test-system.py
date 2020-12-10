#!/usr/bin/env python
from neo4j import debug
import neo4j

debug.watch("neo4j")
def read(tx):
    result = tx.run("SHOW DATABASES")
    for r in result:
        print(r)

with neo4j.GraphDatabase.driver("bolt://localhost:8888", auth=("neo4j", "password")) as driver:
    with driver.session(database="system") as s:
        s.read_transaction(read)

