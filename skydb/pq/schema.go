// Copyright 2015-present Oursky Ltd.
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

package pq

import (
	"bytes"
	"database/sql"
	"fmt"
	"strings"

	"github.com/Sirupsen/logrus"

	"github.com/lib/pq"
	"github.com/skygeario/skygear-server/skydb"
)

func (db *database) Extend(recordType string, recordSchema skydb.RecordSchema) error {
	remoteRecordSchema, err := db.remoteColumnTypes(recordType)
	if err != nil {
		return err
	}

	if len(remoteRecordSchema) == 0 {
		if err := db.createTable(recordType); err != nil {
			return fmt.Errorf("failed to create table: %s", err)
		}
	}
	updatingSchema := skydb.RecordSchema{}
	for key, schema := range recordSchema {
		remoteSchema, ok := remoteRecordSchema[key]
		if !ok {
			updatingSchema[key] = schema
		} else if isConflict(remoteSchema, schema) {
			return fmt.Errorf("conflicting schema %v => %v", remoteSchema, schema)
		}

		// same data type, do nothing
	}

	if len(updatingSchema) > 0 {
		stmt := db.addColumnStmt(recordType, updatingSchema)

		log.WithField("stmt", stmt).Debugln("Adding columns to table")
		if _, err := db.c.Exec(stmt); err != nil {
			return fmt.Errorf("failed to alter table: %s", err)
		}
	}
	delete(db.c.RecordSchema, recordType)
	return nil
}

func (db *database) RenameSchema(recordType, oldName, newName string) error {
	tableName := db.tableName(recordType)
	oldName = pq.QuoteIdentifier(oldName)
	newName = pq.QuoteIdentifier(newName)

	stmt := fmt.Sprintf("ALTER TABLE %s RENAME %s TO %s", tableName, oldName, newName)
	if _, err := db.c.Exec(stmt); err != nil {
		return fmt.Errorf("failed to alter table: %s", err)
	}
	return nil
}

func (db *database) DeleteSchema(recordType, columnName string) error {
	tableName := db.tableName(recordType)
	columnName = pq.QuoteIdentifier(columnName)

	stmt := fmt.Sprintf("ALTER TABLE %s DROP %s", tableName, columnName)
	if _, err := db.c.Exec(stmt); err != nil {
		return fmt.Errorf("failed to alter table: %s", err)
	}
	return nil
}

func (db *database) GetSchema(recordType string) (skydb.RecordSchema, error) {
	remoteRecordSchema, err := db.remoteColumnTypes(recordType)
	if err != nil {
		return nil, err
	}
	return remoteRecordSchema, nil
}

func (db *database) GetRecordSchemas() (map[string]skydb.RecordSchema, error) {
	schemaName := db.schemaName()

	rows, err := db.c.Queryx(`
	SELECT table_name
	FROM information_schema.tables
	WHERE (table_name NOT LIKE '\_%') AND (table_schema=$1)
	`, schemaName)
	if err != nil {
		return nil, err
	}

	result := map[string]skydb.RecordSchema{}
	for rows.Next() {
		var recordType string
		if err := rows.Scan(&recordType); err != nil {
			return nil, err
		}

		log.Debugf("%s\n", recordType)
		schema, err := db.GetSchema(recordType)
		if err != nil {
			return nil, err
		}

		result[recordType] = schema
	}
	log.Debugf("GetRecordSchemas Success")

	return result, nil
}

func (db *database) createTable(recordType string) (err error) {
	tablename := db.tableName(recordType)

	stmt := createTableStmt(tablename)
	log.WithField("stmt", stmt).Debugln("Creating table")
	_, err = db.c.Exec(stmt)
	if err != nil {
		return err
	}

	const CreateTriggerStmtFmt = `CREATE TRIGGER trigger_notify_record_change
    AFTER INSERT OR UPDATE OR DELETE ON %s FOR EACH ROW
    EXECUTE PROCEDURE public.notify_record_change();
`
	stmt = fmt.Sprintf(CreateTriggerStmtFmt, tablename)
	log.WithField("stmt", stmt).Debugln("Creating trigger")
	_, err = db.c.Exec(stmt)

	return err
}

func createTableStmt(tableName string) string {
	return fmt.Sprintf(`
CREATE TABLE %s (
    _id text,
    _database_id text,
    _owner_id text,
    _access jsonb,
    _created_at timestamp without time zone NOT NULL,
    _created_by text,
    _updated_at timestamp without time zone NOT NULL,
    _updated_by text,
    PRIMARY KEY(_id, _database_id, _owner_id),
    UNIQUE (_id)
);
`, tableName)
}

func (db *database) getSequences(recordType string) ([]string, error) {
	const queryString = `
		SELECT c.relname
		FROM pg_catalog.pg_class c
			LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relname LIKE $1 AND n.nspname = $2;
	`

	rows, err := db.c.Queryx(
		queryString,
		fmt.Sprintf("%s\\_%%\\_seq", recordType),
		db.schemaName(),
	)
	if err != nil {
		return []string{}, err
	}

	seqList := []string{}

	for rows.Next() {
		var relname string
		if err = rows.Scan(&relname); err != nil {
			return []string{}, err
		}

		relname = strings.TrimPrefix(relname, fmt.Sprintf("%s_", recordType))
		relname = strings.TrimSuffix(relname, "_seq")

		seqList = append(seqList, relname)
	}

	return seqList, nil
}

// STEP 1 & 2 are obtained by reverse engineering psql \d with -E option
//
// STEP 3: example of getting foreign keys
// SELECT
//     tc.table_name, kcu.column_name,
//     ccu.table_name AS foreign_table_name,
//     ccu.column_name AS foreign_column_name
// FROM
//     information_schema.table_constraints AS tc
//     JOIN information_schema.key_column_usage
//         AS kcu ON tc.constraint_name = kcu.constraint_name
//     JOIN information_schema.constraint_column_usage
//         AS ccu ON ccu.constraint_name = tc.constraint_name
// WHERE constraint_type = 'FOREIGN KEY'
// AND tc.table_schema = 'app__'
// AND tc.table_name = 'note';
func (db *database) remoteColumnTypes(recordType string) (skydb.RecordSchema, error) {
	typemap := skydb.RecordSchema{}
	// STEP 0: Return the cached ColumnType
	if schema, ok := db.c.RecordSchema[recordType]; ok {
		return schema, nil
	}
	defer func() {
		db.c.RecordSchema[recordType] = typemap
		log.Debugf("Cache remoteColumnTypes %s", recordType)
	}()
	log.Debugf("Querying remoteColumnTypes %s", recordType)
	// STEP 1: Get the oid of the current table
	var oid int
	err := db.c.QueryRowx(`
SELECT c.oid
FROM pg_catalog.pg_class c
     LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relname = $1
  AND n.nspname = $2`,
		recordType, db.schemaName()).Scan(&oid)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		log.WithFields(logrus.Fields{
			"schemaName": db.schemaName(),
			"recordType": recordType,
			"err":        err,
		}).Errorln("Failed to query oid of table")
		return nil, err
	}

	// STEP 2: Get column name and data type
	rows, err := db.c.Queryx(`
SELECT a.attname,
  pg_catalog.format_type(a.atttypid, a.atttypmod)
FROM pg_catalog.pg_attribute a
WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped`,
		oid)

	if err != nil {
		log.WithFields(logrus.Fields{
			"schemaName": db.schemaName(),
			"recordType": recordType,
			"oid":        oid,
			"err":        err,
		}).Errorln("Failed to query column and data type")
		return nil, err
	}

	var columnName, pqType string
	var integerColumns = []string{}
	for rows.Next() {
		if err := rows.Scan(&columnName, &pqType); err != nil {
			return nil, err
		}

		schema := skydb.FieldType{}
		switch pqType {
		case TypeString:
			schema.Type = skydb.TypeString
		case TypeNumber:
			schema.Type = skydb.TypeNumber
		case TypeTimestamp:
			schema.Type = skydb.TypeDateTime
		case TypeBoolean:
			schema.Type = skydb.TypeBoolean
		case TypeJSON:
			if columnName == "_access" {
				schema.Type = skydb.TypeACL
			} else {
				schema.Type = skydb.TypeJSON
			}
		case TypeLocation:
			schema.Type = skydb.TypeLocation
		case TypeInteger:
			schema.Type = skydb.TypeInteger
			integerColumns = append(integerColumns, columnName)
		default:
			return nil, fmt.Errorf("received unknown data type = %s for column = %s", pqType, columnName)
		}

		typemap[columnName] = schema
	}

	// STEP 2.1: Convert integer column to sequence column if applicable
	if len(integerColumns) > 0 {
		sequenceList, err := db.getSequences(recordType)
		if err != nil {
			return nil, err
		}

		sequenceMap := map[string]bool{}
		for _, perSeq := range sequenceList {
			sequenceMap[perSeq] = true
		}

		for _, perIntColumn := range integerColumns {
			if _, ok := sequenceMap[perIntColumn]; ok {
				schema := typemap[perIntColumn]
				schema.Type = skydb.TypeSequence

				typemap[perIntColumn] = schema
			}
		}
	}

	// STEP 3: FOREIGN KEY, assumeing we can only reference _id i.e. "ccu.column_name" = _id
	builder := psql.Select("kcu.column_name", "ccu.table_name").
		From("information_schema.table_constraints AS tc").
		Join("information_schema.key_column_usage AS kcu ON tc.constraint_name = kcu.constraint_name").
		Join("information_schema.constraint_column_usage AS ccu ON ccu.constraint_name = tc.constraint_name").
		Where("constraint_type = 'FOREIGN KEY' AND tc.table_schema = ? AND tc.table_name = ?", db.schemaName(), recordType)

	refs, err := db.c.QueryWith(builder)
	if err != nil {
		log.WithFields(logrus.Fields{
			"schemaName": db.schemaName(),
			"recordType": recordType,
			"err":        err,
		}).Errorln("Failed to query foreign key information schema")

		return nil, err
	}

	for refs.Next() {
		s := skydb.FieldType{}
		var primaryColumn, referencedTable string
		if err := refs.Scan(&primaryColumn, &referencedTable); err != nil {
			log.Debugf("err %v", err)
			return nil, err
		}
		switch referencedTable {
		case "_asset":
			s.Type = skydb.TypeAsset
		default:
			s.Type = skydb.TypeReference
			s.ReferenceType = referencedTable
		}
		typemap[primaryColumn] = s
	}
	return typemap, nil
}

func isConflict(from, to skydb.FieldType) bool {
	if from.Type == to.Type {
		return false
	}

	if from.Type.IsNumberCompatibleType() && to.Type.IsNumberCompatibleType() {
		return false
	}

	return true
}

// ALTER TABLE app__.note add collection text;
// ALTER TABLE app__.note
// ADD CONSTRAINT fk_note_collection_collection
// FOREIGN KEY (collection)
// REFERENCES app__.collection(_id);
func (db *database) addColumnStmt(recordType string, recordSchema skydb.RecordSchema) string {
	buf := bytes.Buffer{}
	buf.Write([]byte("ALTER TABLE "))
	buf.WriteString(db.tableName(recordType))
	buf.WriteByte(' ')
	for column, schema := range recordSchema {
		buf.Write([]byte("ADD "))
		buf.WriteString(pq.QuoteIdentifier(column))
		buf.WriteByte(' ')
		buf.WriteString(pqDataType(schema.Type))
		buf.WriteByte(',')
		switch schema.Type {
		case skydb.TypeAsset:
			db.writeForeignKeyConstraint(&buf, column, "_asset", "id")
		case skydb.TypeReference:
			db.writeForeignKeyConstraint(&buf, column, schema.ReferenceType, "_id")
		}
	}

	// remote the last ','
	buf.Truncate(buf.Len() - 1)

	return buf.String()
}

func (db *database) writeForeignKeyConstraint(buf *bytes.Buffer, localCol, referent, remoteCol string) {
	buf.Write([]byte(`ADD CONSTRAINT `))
	buf.WriteString(pq.QuoteIdentifier(fmt.Sprintf(`fk_%s_%s_%s`, localCol, referent, remoteCol)))
	buf.Write([]byte(` FOREIGN KEY (`))
	buf.WriteString(pq.QuoteIdentifier(localCol))
	buf.Write([]byte(`) REFERENCES `))
	buf.WriteString(db.tableName(referent))
	buf.Write([]byte(` (`))
	buf.WriteString(pq.QuoteIdentifier(remoteCol))
	buf.Write([]byte(`),`))
}
