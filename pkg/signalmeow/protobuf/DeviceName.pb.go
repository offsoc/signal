// Code generated by protoc-gen-go. DO NOT EDIT.
// versions:
// 	protoc-gen-go v1.36.6
// 	protoc        v3.21.12
// source: DeviceName.proto

// Copyright 2018 Signal Messenger, LLC
// SPDX-License-Identifier: AGPL-3.0-only

package signalpb

import (
	protoreflect "google.golang.org/protobuf/reflect/protoreflect"
	protoimpl "google.golang.org/protobuf/runtime/protoimpl"
	reflect "reflect"
	sync "sync"
	unsafe "unsafe"
)

const (
	// Verify that this generated code is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(20 - protoimpl.MinVersion)
	// Verify that runtime/protoimpl is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(protoimpl.MaxVersion - 20)
)

type DeviceName struct {
	state           protoimpl.MessageState `protogen:"open.v1"`
	EphemeralPublic []byte                 `protobuf:"bytes,1,opt,name=ephemeralPublic" json:"ephemeralPublic,omitempty"`
	SyntheticIv     []byte                 `protobuf:"bytes,2,opt,name=syntheticIv" json:"syntheticIv,omitempty"`
	Ciphertext      []byte                 `protobuf:"bytes,3,opt,name=ciphertext" json:"ciphertext,omitempty"`
	unknownFields   protoimpl.UnknownFields
	sizeCache       protoimpl.SizeCache
}

func (x *DeviceName) Reset() {
	*x = DeviceName{}
	mi := &file_DeviceName_proto_msgTypes[0]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *DeviceName) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*DeviceName) ProtoMessage() {}

func (x *DeviceName) ProtoReflect() protoreflect.Message {
	mi := &file_DeviceName_proto_msgTypes[0]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use DeviceName.ProtoReflect.Descriptor instead.
func (*DeviceName) Descriptor() ([]byte, []int) {
	return file_DeviceName_proto_rawDescGZIP(), []int{0}
}

func (x *DeviceName) GetEphemeralPublic() []byte {
	if x != nil {
		return x.EphemeralPublic
	}
	return nil
}

func (x *DeviceName) GetSyntheticIv() []byte {
	if x != nil {
		return x.SyntheticIv
	}
	return nil
}

func (x *DeviceName) GetCiphertext() []byte {
	if x != nil {
		return x.Ciphertext
	}
	return nil
}

var File_DeviceName_proto protoreflect.FileDescriptor

const file_DeviceName_proto_rawDesc = "" +
	"\n" +
	"\x10DeviceName.proto\x12\rsignalservice\"x\n" +
	"\n" +
	"DeviceName\x12(\n" +
	"\x0fephemeralPublic\x18\x01 \x01(\fR\x0fephemeralPublic\x12 \n" +
	"\vsyntheticIv\x18\x02 \x01(\fR\vsyntheticIv\x12\x1e\n" +
	"\n" +
	"ciphertext\x18\x03 \x01(\fR\n" +
	"ciphertext"

var (
	file_DeviceName_proto_rawDescOnce sync.Once
	file_DeviceName_proto_rawDescData []byte
)

func file_DeviceName_proto_rawDescGZIP() []byte {
	file_DeviceName_proto_rawDescOnce.Do(func() {
		file_DeviceName_proto_rawDescData = protoimpl.X.CompressGZIP(unsafe.Slice(unsafe.StringData(file_DeviceName_proto_rawDesc), len(file_DeviceName_proto_rawDesc)))
	})
	return file_DeviceName_proto_rawDescData
}

var file_DeviceName_proto_msgTypes = make([]protoimpl.MessageInfo, 1)
var file_DeviceName_proto_goTypes = []any{
	(*DeviceName)(nil), // 0: signalservice.DeviceName
}
var file_DeviceName_proto_depIdxs = []int32{
	0, // [0:0] is the sub-list for method output_type
	0, // [0:0] is the sub-list for method input_type
	0, // [0:0] is the sub-list for extension type_name
	0, // [0:0] is the sub-list for extension extendee
	0, // [0:0] is the sub-list for field type_name
}

func init() { file_DeviceName_proto_init() }
func file_DeviceName_proto_init() {
	if File_DeviceName_proto != nil {
		return
	}
	type x struct{}
	out := protoimpl.TypeBuilder{
		File: protoimpl.DescBuilder{
			GoPackagePath: reflect.TypeOf(x{}).PkgPath(),
			RawDescriptor: unsafe.Slice(unsafe.StringData(file_DeviceName_proto_rawDesc), len(file_DeviceName_proto_rawDesc)),
			NumEnums:      0,
			NumMessages:   1,
			NumExtensions: 0,
			NumServices:   0,
		},
		GoTypes:           file_DeviceName_proto_goTypes,
		DependencyIndexes: file_DeviceName_proto_depIdxs,
		MessageInfos:      file_DeviceName_proto_msgTypes,
	}.Build()
	File_DeviceName_proto = out.File
	file_DeviceName_proto_goTypes = nil
	file_DeviceName_proto_depIdxs = nil
}
