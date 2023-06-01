package bigproto

import (
	"context"
	"errors"
	"fmt"
	"go.alis.build/alog"
	"google.golang.org/protobuf/types/known/timestamppb"
	"log"
	"os"
	"reflect"
	"strings"

	"cloud.google.com/go/bigtable"
	"github.com/imdario/mergo"
	"github.com/mennanov/fmutils"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// ErrNotFound is returned when the desired resource is not found in Bigtable.
type ErrNotFound struct {
	RowKey string // unavailable locations
}

func (e ErrNotFound) Error() string {
	return fmt.Sprintf("%s not found", e.RowKey)
}

var ErrInvalidFieldMask = errors.New("invalid field mask")

type ErrMismatchedTypes struct {
	Expected reflect.Type
	Actual   reflect.Type
}

func (e ErrMismatchedTypes) Error() string {
	return fmt.Sprintf("expected %s, got %s", e.Expected, e.Actual)
}

const DefaultColumnName = "0"

type BigProto struct {
	table *bigtable.Table
}

// New does the same as NewClient, except that it allows you to pass in the bigtable client directly, instead of passing in the project, instance and table name.
func New(client *bigtable.Client, tableName string) *BigProto {
	table := client.Open(tableName)
	return &BigProto{
		table: table,
	}
}

// NewClient returns a bigproto object, containing an initialized bigtable connection using the project,instance and table name as connection parameters
// It is recommended that you call this function once in your package's init function and then store the returned object as a global variable, instead of making new connections with every read/write.
func NewClient(ctx context.Context, googleProject string, bigTableInstance string, tableName string) *BigProto {
	client, err := bigtable.NewClient(ctx, googleProject, bigTableInstance)
	if err != nil {
		alog.Fatalf(ctx, "Error creating bigtable client: %s", err)
	}
	return New(client, tableName)
}

// SetupAndUseBigtableEmulator ensures that any other calls from the bigtable client are made to the gcloud bigtable emulator running on your local machine. This makes it possible to test your code without needing to setup an actual bigtable instance in the cloud.
func SetupAndUseBigtableEmulator(googleProject string, bigTableInstance string, tableName string, columnFamilies []string, createIfNotExist bool, resetIfExist bool) {
	//set environment variable that will make the bigtable client connect to local bigtable
	_ = os.Setenv("BIGTABLE_EMULATOR_HOST", "localhost:8086")

	// initialize admin client to create and/or delete table
	adminClient, err := bigtable.NewAdminClient(context.Background(), googleProject, bigTableInstance)
	if err != nil {
		log.Fatalf("Could not create admin client: %v", err)
	}

	// delete table if required to reset it
	if resetIfExist {
		err = adminClient.DeleteTable(context.Background(), tableName)
		if strings.Contains(err.Error(), "connection refused") {
			panic("Bigtable emulator not running. Run 'gcloud beta emulators bigtable start'")
		}
	}

	// create table if create/reset is required
	if createIfNotExist || resetIfExist {
		err = adminClient.CreateTable(context.Background(), tableName)
		if err != nil && !strings.Contains(err.Error(), "already exists") {
			// if the emulator has not been started up, instruct the developer how to do this
			if strings.Contains(err.Error(), "connection refused") {
				panic("Bigtable emulator not running. Run 'gcloud beta emulators bigtable start'")
			}
			panic(err)
		}
	}

	// create column families that do not already exist
	for _, cf := range columnFamilies {
		err = adminClient.CreateColumnFamily(context.Background(), tableName, cf)
		if err != nil && !strings.Contains(err.Error(), "already exists") {
			if strings.Contains(err.Error(), "connection refused") {
				panic("Bigtable emulator not running. Run 'gcloud beta emulators bigtable start'")
			}
			panic(err)
		}
	}
}

// WriteProto writes the provided proto message to Bigtable by marshaling it to bytes and storing the data at the given
// row key, and column family.
func (b *BigProto) WriteProto(ctx context.Context, rowKey string, columnFamily string, message proto.Message) error {
	timestamp := bigtable.Now()

	dataBytes, err := proto.Marshal(message)
	if err != nil {
		return err
	}

	mut := bigtable.NewMutation()
	mut.Set(columnFamily, DefaultColumnName, timestamp, dataBytes)
	err = b.table.Apply(ctx, rowKey, mut)
	if err != nil {
		return err
	}
	return nil
}

// ReadProto obtains a Bigtable row entry, unmarshalls the value at the given columnFamily, applies the read mask and
// stores the result in the provided message pointer.
func (b *BigProto) ReadProto(ctx context.Context, rowKey string, columnFamily string, message proto.Message, readMask *fieldmaskpb.FieldMask) error {
	// retrieve the resource from bigtable
	filter := bigtable.ChainFilters(bigtable.LatestNFilter(1), bigtable.FamilyFilter(columnFamily))
	row, err := b.table.ReadRow(ctx, rowKey, bigtable.RowFilter(filter))
	if err != nil {
		return err
	}
	if row == nil {
		return ErrNotFound{RowKey: rowKey}
	}

	// Each collection is stored in a corresponding Bigtable family
	columns, ok := row[columnFamily]
	if !ok {
		return ErrNotFound{RowKey: rowKey}
	}

	// if there are no results in the row, exit and return a nil Map.
	if len(columns) == 0 {
		return ErrNotFound{RowKey: rowKey}
	}

	// Only the first column is used by the resource.
	column := columns[0]
	err = proto.Unmarshal(column.Value, message)
	if err != nil {
		return err
	}

	// Apply Read Mask if provided
	if readMask != nil {
		readMask.Normalize()
		if !readMask.IsValid(message) {
			return ErrInvalidFieldMask
		}
		// Redact the request according to the provided field mask.
		fmutils.Filter(message, readMask.GetPaths())
	}

	return nil
}

// UpdateProto obtains a Bigtable row entry and unmarshalls the value at the given columnFamily to the type provided. It
// then merges the updates as specified in the provided message, into the current type, in line with the update mask
// and writes the updated proto back to Bigtable. The updated proto is also stored in the provided message pointer.
func (b *BigProto) UpdateProto(ctx context.Context, rowKey string, columnFamily string, message proto.Message, updateMask *fieldmaskpb.FieldMask) error {
	// retrieve the resource from bigtable
	currentMessage := newEmptyMessage(message)
	err := b.ReadProto(ctx, rowKey, columnFamily, currentMessage, nil)
	if err != nil {
		return err
	}

	// merge the updates into currentMessage
	err = mergeUpdates(currentMessage, message, updateMask)
	if err != nil {
		return err
	}

	// write the updated message back to bigtable
	err = b.WriteProto(ctx, rowKey, columnFamily, currentMessage)
	if err != nil {
		return err
	}
	// update the message pointer
	reflect.ValueOf(message).Elem().Set(reflect.ValueOf(currentMessage).Elem())

	return nil
}

// newEmptyMessage returns a new instance of the same type as the provided proto.Message
func newEmptyMessage(msg proto.Message) proto.Message {
	// Get the reflect.Type of the message
	msgType := reflect.TypeOf(msg)
	if msgType.Kind() == reflect.Ptr {
		msgType = msgType.Elem()
	}

	// Create a new instance of the message type using reflection
	newMsg := reflect.New(msgType).Interface().(proto.Message)
	return newMsg
}

// mergeUpdates merges the updates into the current message in line with the update mask
func mergeUpdates(current proto.Message, updates proto.Message, updateMask *fieldmaskpb.FieldMask) error {
	// If current and updates are different types, return an error
	if reflect.TypeOf(current) != reflect.TypeOf(updates) {
		return ErrMismatchedTypes{
			Expected: reflect.TypeOf(current),
			Actual:   reflect.TypeOf(updates),
		}
	}

	// If updates is nil, return nil
	if updates == nil {
		return nil
	}
	// If current is nil, return updates
	if current == nil {
		current = updates
		return nil
	}

	// If updates is empty, return nil
	if proto.Size(updates) == 0 {
		return nil
	}

	// Apply Update Mask if provided
	if updateMask != nil {
		updateMask.Normalize()
		if !updateMask.IsValid(current) {
			return ErrInvalidFieldMask
		}
		// Redact the request according to the provided field mask.
		fmutils.Prune(current, updateMask.GetPaths())
	}

	// Merge the updates into the current message
	err := mergo.Merge(current, updates)
	if err != nil {
		return err
	}

	return nil
}

// ReadRow returns the row from bigtable at the given rowKey. This allows for more custom read functionality to be
// implemented on the row that is returned. This is useful for reading multiple columns from a row, or reading a row
// with a filter. It also allows for things like "Source Prioritisation" whereby data may be duplicated across column
// families for different sources and the sources are used in order of prior
func (b *BigProto) ReadRow(ctx context.Context, rowKey string) (bigtable.Row, error) {
	// retrieve the resource from bigtable
	row, err := b.table.ReadRow(ctx, rowKey)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, ErrNotFound{RowKey: rowKey}
	}

	return row, nil
}

// WriteMutation writes a mutation to bigtable at the given rowKey. This allows for more custom write functionality to
// be implemented on the row that is written. This is useful for writing multiple columns to a row, or writing a row
// with a filter. It also allows for things like "Source Prioritisation" whereby data may be duplicated across column
// families for different sources and the sources are used in order of prior
func (b *BigProto) WriteMutation(ctx context.Context, rowKey string, mut *bigtable.Mutation) error {
	err := b.table.Apply(ctx, rowKey, mut)
	if err != nil {
		return err
	}
	return nil
}

// DeleteRow deletes an entire row from bigtable at the given rowKey.
func (b *BigProto) DeleteRow(ctx context.Context, rowKey string) error {

	// Create a single mutation to delete the row
	mut := bigtable.NewMutation()
	mut.DeleteRow()
	err := b.table.Apply(ctx, rowKey, mut)
	if err != nil {
		return fmt.Errorf("delete bigtable row: %w", err)
	}
	return nil
}

// ListProtos returns the list of rows for a specified set of rows
func (b *BigProto) ListProtos(ctx context.Context, columnFamily string, messageType proto.Message, readMask *fieldmaskpb.FieldMask, rowSet bigtable.RowSet, opts ...bigtable.ReadOption) ([]proto.Message, error) {
	var res []proto.Message

	// Validate readMask if provided
	if readMask != nil {
		readMask.Normalize()
		if !readMask.IsValid(messageType) {
			return nil, ErrInvalidFieldMask
		}
	}

	err := b.table.ReadRows(ctx, rowSet,
		func(row bigtable.Row) bool {

			// if the row is empty, append an empty value and continue
			if row == nil {
				res = append(res, nil)
				return true
			}

			// Each collection is stored in a corresponding Bigtable family
			columns := row[columnFamily]

			// if there are no results in the row, append an empty value and continue
			if len(columns) == 0 {
				res = append(res, nil)
				return true
			}

			// only the first column is used by the resource.
			column := columns[0]
			var message proto.Message
			err := proto.Unmarshal(column.Value, messageType)
			if err != nil {
				return false
			}
			message = proto.Clone(messageType)
			if message != nil {
				// Apply Read Mask if provided
				if readMask != nil {
					// Redact the request according to the provided field mask.
					fmutils.Filter(message, readMask.GetPaths())
				}
				res = append(res, message)
			}
			return true
		},
		opts...,
	)
	if err != nil {
		return nil, err
	}

	return res, nil
}

// Now returns the time using Bigtable's time method.
func Now() *timestamppb.Timestamp {
	return timestamppb.New(bigtable.Now().Time())
}
