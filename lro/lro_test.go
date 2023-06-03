package lro

import (
	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/bigtable/bttest"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"context"
	"google.golang.org/api/option"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"log"
	"strconv"
	"testing"
)

const tableName = "test"

var Table *bigtable.Table
var families = []string{
	ColumnFamily,
}

func init() {
	ctx := context.Background()
	srv, err := bttest.NewServer("localhost:0")
	if err != nil {
		log.Fatal(err)
	}

	conn, err := grpc.Dial(srv.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}

	proj, instance := "proj", "instance"
	adminClient, err := bigtable.NewAdminClient(ctx, proj, instance, option.WithGRPCConn(conn))
	if err != nil {
		log.Fatal(err)
	}

	tables := []string{tableName}
	for _, ta := range tables {
		if err = adminClient.CreateTable(ctx, ta); err != nil {
			log.Fatal(err)
		}
		for _, f := range families {
			if err = adminClient.CreateColumnFamily(ctx, ta, f); err != nil {
				log.Fatal(err)
			}
		}
	}

	client, err := bigtable.NewClient(ctx, proj, instance, option.WithGRPCConn(conn))
	if err != nil {
		log.Fatal(err)
	}

	Table = client.Open(tableName)
}

func TestLroClient_CreateOperation(t *testing.T) {
	type fields struct {
		t *bigtable.Table
	}
	type args struct {
		ctx  context.Context
		opts CreateOpts
	}
	tests := []struct {
		name            string
		fields          fields
		args            args
		want            *longrunningpb.Operation
		wantErr         bool
		useResponseName bool
	}{
		{
			name: "auto-generated id with no parent or metatdata",
			fields: fields{
				t: Table,
			},
			args: args{
				ctx: context.Background(),
				opts: CreateOpts{
					id:       "",
					parent:   "",
					metadata: nil,
				},
			},
			want: &longrunningpb.Operation{
				//name is auto-generated
			},
			wantErr:         false,
			useResponseName: true,
		},
		{
			name: "set id with parent and no metadata",
			fields: fields{
				t: Table,
			},
			args: args{
				ctx: context.Background(),
				opts: CreateOpts{
					id:       "test-id",
					parent:   "test-parent",
					metadata: nil,
				},
			},
			want: &longrunningpb.Operation{
				Name: "operations/test-id",
			},
			wantErr:         false,
			useResponseName: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := &LroClient{
				t: tt.fields.t,
			}
			got, err := l.CreateOperation(tt.args.ctx, tt.args.opts)
			if (err != nil) != tt.wantErr {
				t.Errorf("CreateOperation() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.useResponseName {
				tt.want.Name = got.Name
			}
			if !proto.Equal(got, tt.want) {
				t.Errorf("CreateOperation() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLroClient_GetOperation(t *testing.T) {
	l := &LroClient{
		t: Table,
	}
	// arrange by creating 5 test operations
	create5TestOperations(l)

	// act and assert
	type args struct {
		ctx           context.Context
		operationName string
	}
	tests := []struct {
		name    string
		args    args
		want    *longrunningpb.Operation
		wantErr bool
	}{
		{
			name: "basic",
			args: args{
				ctx:           context.Background(),
				operationName: "operations/test-id-1",
			},
			want:    &longrunningpb.Operation{Name: "operations/test-id-1"},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			got, err := l.GetOperation(tt.args.ctx, tt.args.operationName)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetOperation() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !proto.Equal(got, tt.want) {
				t.Errorf("GetOperation() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLroClient_SetSuccessful(t *testing.T) {
	type args struct {
		ctx           context.Context
		operationName string
		response      *anypb.Any
		metaOptions   MetaOptions
	}
	l := &LroClient{
		t: Table,
	}
	// arrange
	create5TestOperations(l)
	tests := []struct {
		name    string
		args    args
		wantErr bool
	}{
		{
			name: "existing operation",
			args: args{
				ctx:           context.Background(),
				operationName: "operations/test-id-1",
				response:      nil,
				metaOptions:   MetaOptions{update: true, newMetaData: &longrunningpb.Operation{}},
			},
			wantErr: false,
		},
		{
			name: "non-existant operation",
			args: args{
				ctx:           context.Background(),
				operationName: "operations/asdf",
				response:      nil,
			},
			wantErr: true,
		},
	}

	// act and assert
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			if err := l.SetSuccessful(tt.args.ctx, tt.args.operationName, tt.args.response, tt.args.metaOptions); (err != nil) != tt.wantErr {
				t.Errorf("SetSuccessful() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				op, _ := l.GetOperation(tt.args.ctx, tt.args.operationName)
				if op.Done != true {
					t.Errorf("SetSuccessful() did not update the done field to true")
				}
				if op.GetResult() == nil {
					t.Errorf("SetFailed() did not update the result field")
				}
			}
		})
	}
}

func TestLroClient_SetFailed(t *testing.T) {
	type args struct {
		ctx           context.Context
		operationName string
		response      *anypb.Any
	}
	l := &LroClient{
		t: Table,
	}
	// arrange
	create5TestOperations(l)
	tests := []struct {
		name    string
		args    args
		wantErr bool
	}{
		{
			name: "existing operation",
			args: args{
				ctx:           context.Background(),
				operationName: "operations/test-id-1",
				response:      nil,
			},
			wantErr: false,
		},
		{
			name: "non-existant operation",
			args: args{
				ctx:           context.Background(),
				operationName: "operations/asdf",
				response:      nil,
			},
			wantErr: true,
		},
	}

	// act and assert
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			if err := l.SetFailed(tt.args.ctx, tt.args.operationName, &status.Status{}, MetaOptions{}); (err != nil) != tt.wantErr {
				t.Errorf("SetFailed() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				op, _ := l.GetOperation(tt.args.ctx, tt.args.operationName)
				if op.Done != true {
					t.Errorf("SetFailed() did not update the done field to true")
				}
				if op.GetResult() == nil {
					t.Errorf("SetFailed() did not update the result field")
				}
			}
		})
	}
}

func create5TestOperations(l *LroClient) {
	for i := 0; i < 5; i++ {
		_, _ = l.CreateOperation(context.Background(), CreateOpts{id: "test-id-" + strconv.FormatInt(int64(i), 10), parent: "test-parent", metadata: nil})
	}
}
