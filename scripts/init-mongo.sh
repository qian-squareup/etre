#!/bin/bash

echo "Waiting for MongoDB to be ready..."
until mongosh --eval "db.runCommand('ping').ok" > /dev/null 2>&1; do
    sleep 2
done

echo "Initializing replica set..."
mongosh --eval '
  rs.initiate({
    _id: "rs0",
    members: [
      { _id: 0, host: "localhost:27017" }
    ]
  })
'

echo "Waiting for replica set to be ready..."
until mongosh --eval "rs.isMaster().ismaster" | grep -q "true"; do
    sleep 2
done

echo "MongoDB replica set is ready!"