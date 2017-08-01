package mqswag

import (
	"errors"
	"meqa/mqutil"
	"reflect"
	"sync"

	"github.com/go-openapi/spec"
	"github.com/xeipuuv/gojsonschema"
)

// This file implements the in-memory DB that holds all the entity objects.

// Schema is the swagger spec schema.
type Schema spec.Schema

// Matches checks if the Schema matches the input interface. In proper swagger.json
// Enums should have types as well. So we don't check for untyped enums.
// TODO check format, handle AllOf, AnyOf, OneOf
func (schema *Schema) Matches(object interface{}) bool {
	if object == nil {
		return schema.Type[0] == gojsonschema.TYPE_NULL
	}

	k := reflect.TypeOf(object).Kind()
	if k == reflect.Bool {
		return schema.Type[0] == gojsonschema.TYPE_BOOLEAN
	} else if k >= reflect.Int && k <= reflect.Uint64 {
		return schema.Type[0] == gojsonschema.TYPE_INTEGER || schema.Type[0] == gojsonschema.TYPE_NUMBER
	} else if k == reflect.Float32 || k == reflect.Float64 {
		return schema.Type[0] == gojsonschema.TYPE_NUMBER
	} else if k == reflect.Array || k == reflect.Slice {
		if schema.Type[0] != gojsonschema.TYPE_ARRAY {
			return false
		}
		// Check the array elements.
		itemsSchema := (*Schema)(schema.Items.Schema)
		ar := object.([]interface{})
		for _, item := range ar {
			if !itemsSchema.Matches(item) {
				return false
			}
		}
		return true
	} else if k == reflect.Map {
		if schema.Type[0] != gojsonschema.TYPE_OBJECT {
			return false
		}
		// check the object content.
		return schema.MatchesMap(object.(map[string]interface{}))
	} else if k == reflect.String {
		if schema.Type[0] == gojsonschema.TYPE_STRING {
			return true
		}
	} else {
		mqutil.Logger.Printf("unknown type: %v", k)
	}
	return false
}

// MatchesMap checks if the Schema matches the input map.
func (schema *Schema) MatchesMap(obj map[string]interface{}) bool {
	if obj == nil {
		return false
	}
	// check all required fields in Schema are present in the object.
	for _, requiredName := range schema.Required {
		if obj[requiredName] == nil {
			return false
		}
	}
	// check all object's fields are in schema and the types match.
	for k, v := range obj {
		if p, ok := schema.Properties[k]; ok {
			if !((*Schema)(&p)).Matches(v) {
				return false
			}
		}
	}
	return true
}

// SchemaDB is our in-memory DB. It is organized around Schemas. Each schema maintains a list of objects that matches
// the schema. We don't build indexes and do linear search. This keeps the searching flexible for now.
type SchemaDB struct {
	Name    string
	Schema  *Schema
	Objects []interface{}
}

// Insert inserts an object into the schema's object list.
func (db *SchemaDB) Insert(obj interface{}) error {
	if !db.Schema.Matches(obj) {
		return errors.New("object and schema doesn't match")
	}
	db.Objects = append(db.Objects, obj)
	return nil
}

// MatchFunc checks whether the input criteria and an input object matches.
type MatchFunc func(criteria interface{}, existing interface{}) bool

// An implementation of the MatchFunc that returns true if the existing object matches all the fields in the criteria obj.
func MatchAllFields(criteria interface{}, existing interface{}) bool {
	cm, ok := criteria.(map[string]interface{})
	if !ok {
		return false
	}
	em, ok := existing.(map[string]interface{})
	if !ok {
		return false
	}
	// We only do simple value comparision for now. We know that our search keys are simple types.
	for k, v := range cm {
		if em[k] != v {
			return false
		}
	}
	return true
}

// Find finds the specified number of objects that match the input criteria.
func (db *SchemaDB) Find(criteria interface{}, matches MatchFunc, desiredCount int) []interface{} {
	var result []interface{}
	for _, obj := range db.Objects {
		if matches(criteria, obj) {
			result = append(result, obj)
			if len(result) >= desiredCount {
				return result
			}
		}
	}
	return result
}

// Delete deletes the specified number of elements that match the criteria. Input -1 for delete all.
// Returns the number of elements deleted.
func (db *SchemaDB) Delete(criteria interface{}, matches MatchFunc, desiredCount int) int {
	count := 0
	for i, obj := range db.Objects {
		if matches(criteria, obj) {
			db.Objects[i] = db.Objects[count]
			count++
			if count >= desiredCount {
				break
			}
		}
	}
	db.Objects = db.Objects[count:]
	return count
}

// Update finds the matching object, then update with the new one.
func (db *SchemaDB) Update(criteria interface{}, matches MatchFunc, newObj map[string]interface{}, desiredCount int, patch bool) int {
	count := 0
	for i, obj := range db.Objects {
		if matches(criteria, obj) {
			m, ok := obj.(map[string]interface{})
			if !ok {
				continue
			}
			if patch {
				mqutil.MapCombine(m, newObj)
			} else {
				db.Objects[i] = newObj
			}
			count++
			if count >= desiredCount {
				break
			}
		}
	}
	db.Objects = db.Objects[count:]
	return count
}

type DB struct {
	schemas map[string](*SchemaDB)
	Swagger *Swagger
	mutex   sync.Mutex // We don't expect much contention, as such mutex will be fast
}

func (db *DB) Init(s *Swagger) {
	db.Swagger = s
	db.schemas = make(map[string](*SchemaDB))
	for schemaName, schema := range s.Definitions {
		if _, ok := db.schemas[schemaName]; ok {
			mqutil.Logger.Printf("warning - schema %s already exists", schemaName)
		}
		db.schemas[schemaName] = &SchemaDB{schemaName, (*Schema)(&schema), nil}
	}
}

func (db *DB) Insert(name string, schema *spec.Schema, obj interface{}) error {
	db.mutex.Lock()
	defer db.mutex.Unlock()

	if db.schemas[name] == nil {
		db.schemas[name] = &SchemaDB{name, (*Schema)(schema), nil}
	}
	return db.schemas[name].Insert(obj)
}

func (db *DB) Find(name string, criteria interface{}, matches MatchFunc, desiredCount int) []interface{} {
	db.mutex.Lock()
	defer db.mutex.Unlock()

	if db.schemas[name] == nil {
		return nil
	}
	return db.schemas[name].Find(criteria, matches, desiredCount)
}

func (db *DB) Delete(name string, criteria interface{}, matches MatchFunc, desiredCount int) int {
	db.mutex.Lock()
	defer db.mutex.Unlock()

	if db.schemas[name] == nil {
		return 0
	}
	return db.schemas[name].Delete(criteria, matches, desiredCount)
}

func (db *DB) Update(name string, criteria interface{}, matches MatchFunc, newObj map[string]interface{}, desiredCount int, patch bool) int {
	db.mutex.Lock()
	defer db.mutex.Unlock()

	if db.schemas[name] == nil {
		return 0
	}
	return db.schemas[name].Update(criteria, matches, newObj, desiredCount, patch)
}

// FindMatchingSchema finds the schema that matches the obj.
func (db *DB) FindMatchingSchema(obj interface{}) (string, *spec.Schema) {
	for name, schemaDB := range db.schemas {
		schema := schemaDB.Schema
		if schema.Matches(obj) {
			mqutil.Logger.Printf("found matching schema: %s", name)
			return name, (*spec.Schema)(schema)
		}
	}
	return "", nil
}

// DB holds schema name to Schema mapping.
var ObjDB DB
