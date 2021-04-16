// Copyright 2017-2018 New Vector Ltd
// Copyright 2019-2020 The Matrix.org Foundation C.I.C.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sqlite3

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/matrix-org/dendrite/internal"
	"github.com/matrix-org/dendrite/internal/sqlutil"
	"github.com/matrix-org/dendrite/roomserver/storage/shared"
	"github.com/matrix-org/dendrite/roomserver/storage/tables"
	"github.com/matrix-org/dendrite/roomserver/types"
	"github.com/matrix-org/util"
)

const stateSnapshotSchema = `
  CREATE TABLE IF NOT EXISTS roomserver_state_snapshots (
    state_snapshot_nid INTEGER PRIMARY KEY AUTOINCREMENT,
    room_nid INTEGER NOT NULL,
    state_block_nids TEXT NOT NULL DEFAULT '[]',
	UNIQUE (room_nid, state_block_nids)
  );
`

const insertStateSQL = `
	INSERT INTO roomserver_state_snapshots (room_nid, state_block_nids)
	  VALUES ($1, $2)
	  ON CONFLICT (room_nid, state_block_nids) DO UPDATE SET room_nid=$3
	  RETURNING state_snapshot_nid
`

// Bulk state data NID lookup.
// Sorting by state_snapshot_nid means we can use binary search over the result
// to lookup the state data NIDs for a state snapshot NID.
const bulkSelectStateBlockNIDsSQL = "" +
	"SELECT state_snapshot_nid, state_block_nids FROM roomserver_state_snapshots" +
	" WHERE state_snapshot_nid IN ($1) ORDER BY state_snapshot_nid ASC"

type stateSnapshotStatements struct {
	db                           *sql.DB
	insertStateStmt              *sql.Stmt
	bulkSelectStateBlockNIDsStmt *sql.Stmt
}

func NewSqliteStateSnapshotTable(db *sql.DB) (tables.StateSnapshot, error) {
	s := &stateSnapshotStatements{
		db: db,
	}
	_, err := db.Exec(stateSnapshotSchema)
	if err != nil {
		return nil, err
	}

	return s, shared.StatementList{
		{&s.insertStateStmt, insertStateSQL},
		{&s.bulkSelectStateBlockNIDsStmt, bulkSelectStateBlockNIDsSQL},
	}.Prepare(db)
}

func (s *stateSnapshotStatements) InsertState(
	ctx context.Context, txn *sql.Tx, roomNID types.RoomNID, stateBlockNIDs types.StateBlockNIDs,
) (stateNID types.StateSnapshotNID, err error) {
	stateBlockNIDs = stateBlockNIDs[:util.SortAndUnique(stateBlockNIDs)]
	stateBlockNIDsJSON, err := json.Marshal(stateBlockNIDs)
	if err != nil {
		return
	}
	insertStmt := sqlutil.TxStmt(txn, s.insertStateStmt)
	var id int64
	err = insertStmt.QueryRowContext(ctx, int64(roomNID), string(stateBlockNIDsJSON), int64(roomNID)).Scan(&id)
	if err != nil {
		return 0, err
	}
	stateNID = types.StateSnapshotNID(id)
	return
}

func (s *stateSnapshotStatements) BulkSelectStateBlockNIDs(
	ctx context.Context, stateNIDs []types.StateSnapshotNID,
) ([]types.StateBlockNIDList, error) {
	nids := make([]interface{}, len(stateNIDs))
	for k, v := range stateNIDs {
		nids[k] = v
	}
	selectOrig := strings.Replace(bulkSelectStateBlockNIDsSQL, "($1)", sqlutil.QueryVariadic(len(nids)), 1)
	selectStmt, err := s.db.Prepare(selectOrig)
	if err != nil {
		return nil, err
	}

	rows, err := selectStmt.QueryContext(ctx, nids...)
	if err != nil {
		return nil, err
	}
	defer internal.CloseAndLogIfError(ctx, rows, "bulkSelectStateBlockNIDs: rows.close() failed")
	results := make([]types.StateBlockNIDList, len(stateNIDs))
	i := 0
	for ; rows.Next(); i++ {
		result := &results[i]
		var stateBlockNIDsJSON string
		if err := rows.Scan(&result.StateSnapshotNID, &stateBlockNIDsJSON); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(stateBlockNIDsJSON), &result.StateBlockNIDs); err != nil {
			return nil, err
		}
	}
	if i != len(stateNIDs) {
		return nil, fmt.Errorf("storage: state NIDs missing from the database (%d != %d)", i, len(stateNIDs))
	}
	return results, nil
}