// Code generated by protoc-gen-gogo.
// source: cockroach/pkg/ccl/utilccl/license.proto
// DO NOT EDIT!

/*
	Package utilccl is a generated protocol buffer package.

	It is generated from these files:
		cockroach/pkg/ccl/utilccl/license.proto

	It has these top-level messages:
		License
*/
package utilccl

import proto "github.com/gogo/protobuf/proto"
import fmt "fmt"
import math "math"

import github_com_cockroachdb_cockroach_pkg_util_uuid "github.com/cockroachdb/cockroach/pkg/util/uuid"

import io "io"

// Reference imports to suppress errors if they are not otherwise used.
var _ = proto.Marshal
var _ = fmt.Errorf
var _ = math.Inf

// This is a compile-time assertion to ensure that this generated file
// is compatible with the proto package it is being compiled against.
// A compilation error at this line likely means your copy of the
// proto package needs to be updated.
const _ = proto.GoGoProtoPackageIsVersion2 // please upgrade the proto package

type License_Type int32

const (
	License_NonCommercial License_Type = 0
	License_Enterprise    License_Type = 1
	License_Evaluation    License_Type = 2
)

var License_Type_name = map[int32]string{
	0: "NonCommercial",
	1: "Enterprise",
	2: "Evaluation",
}
var License_Type_value = map[string]int32{
	"NonCommercial": 0,
	"Enterprise":    1,
	"Evaluation":    2,
}

func (x License_Type) String() string {
	return proto.EnumName(License_Type_name, int32(x))
}
func (License_Type) EnumDescriptor() ([]byte, []int) { return fileDescriptorLicense, []int{0, 0} }

type License struct {
	ClusterID         []github_com_cockroachdb_cockroach_pkg_util_uuid.UUID `protobuf:"bytes,1,rep,name=cluster_id,json=clusterId,customtype=github.com/cockroachdb/cockroach/pkg/util/uuid.UUID" json:"cluster_id"`
	ValidUntilUnixSec int64                                                 `protobuf:"varint,2,opt,name=valid_until_unix_sec,json=validUntilUnixSec,proto3" json:"valid_until_unix_sec,omitempty"`
	Type              License_Type                                          `protobuf:"varint,3,opt,name=type,proto3,enum=cockroach.ccl.utilccl.License_Type" json:"type,omitempty"`
	OrganizationName  string                                                `protobuf:"bytes,4,opt,name=organization_name,json=organizationName,proto3" json:"organization_name,omitempty"`
}

func (m *License) Reset()                    { *m = License{} }
func (m *License) String() string            { return proto.CompactTextString(m) }
func (*License) ProtoMessage()               {}
func (*License) Descriptor() ([]byte, []int) { return fileDescriptorLicense, []int{0} }

func init() {
	proto.RegisterType((*License)(nil), "cockroach.ccl.utilccl.License")
	proto.RegisterEnum("cockroach.ccl.utilccl.License_Type", License_Type_name, License_Type_value)
}
func (m *License) Marshal() (dAtA []byte, err error) {
	size := m.Size()
	dAtA = make([]byte, size)
	n, err := m.MarshalTo(dAtA)
	if err != nil {
		return nil, err
	}
	return dAtA[:n], nil
}

func (m *License) MarshalTo(dAtA []byte) (int, error) {
	var i int
	_ = i
	var l int
	_ = l
	if len(m.ClusterID) > 0 {
		for _, msg := range m.ClusterID {
			dAtA[i] = 0xa
			i++
			i = encodeVarintLicense(dAtA, i, uint64(msg.Size()))
			n, err := msg.MarshalTo(dAtA[i:])
			if err != nil {
				return 0, err
			}
			i += n
		}
	}
	if m.ValidUntilUnixSec != 0 {
		dAtA[i] = 0x10
		i++
		i = encodeVarintLicense(dAtA, i, uint64(m.ValidUntilUnixSec))
	}
	if m.Type != 0 {
		dAtA[i] = 0x18
		i++
		i = encodeVarintLicense(dAtA, i, uint64(m.Type))
	}
	if len(m.OrganizationName) > 0 {
		dAtA[i] = 0x22
		i++
		i = encodeVarintLicense(dAtA, i, uint64(len(m.OrganizationName)))
		i += copy(dAtA[i:], m.OrganizationName)
	}
	return i, nil
}

func encodeFixed64License(dAtA []byte, offset int, v uint64) int {
	dAtA[offset] = uint8(v)
	dAtA[offset+1] = uint8(v >> 8)
	dAtA[offset+2] = uint8(v >> 16)
	dAtA[offset+3] = uint8(v >> 24)
	dAtA[offset+4] = uint8(v >> 32)
	dAtA[offset+5] = uint8(v >> 40)
	dAtA[offset+6] = uint8(v >> 48)
	dAtA[offset+7] = uint8(v >> 56)
	return offset + 8
}
func encodeFixed32License(dAtA []byte, offset int, v uint32) int {
	dAtA[offset] = uint8(v)
	dAtA[offset+1] = uint8(v >> 8)
	dAtA[offset+2] = uint8(v >> 16)
	dAtA[offset+3] = uint8(v >> 24)
	return offset + 4
}
func encodeVarintLicense(dAtA []byte, offset int, v uint64) int {
	for v >= 1<<7 {
		dAtA[offset] = uint8(v&0x7f | 0x80)
		v >>= 7
		offset++
	}
	dAtA[offset] = uint8(v)
	return offset + 1
}
func (m *License) Size() (n int) {
	var l int
	_ = l
	if len(m.ClusterID) > 0 {
		for _, e := range m.ClusterID {
			l = e.Size()
			n += 1 + l + sovLicense(uint64(l))
		}
	}
	if m.ValidUntilUnixSec != 0 {
		n += 1 + sovLicense(uint64(m.ValidUntilUnixSec))
	}
	if m.Type != 0 {
		n += 1 + sovLicense(uint64(m.Type))
	}
	l = len(m.OrganizationName)
	if l > 0 {
		n += 1 + l + sovLicense(uint64(l))
	}
	return n
}

func sovLicense(x uint64) (n int) {
	for {
		n++
		x >>= 7
		if x == 0 {
			break
		}
	}
	return n
}
func sozLicense(x uint64) (n int) {
	return sovLicense(uint64((x << 1) ^ uint64((int64(x) >> 63))))
}
func (m *License) Unmarshal(dAtA []byte) error {
	l := len(dAtA)
	iNdEx := 0
	for iNdEx < l {
		preIndex := iNdEx
		var wire uint64
		for shift := uint(0); ; shift += 7 {
			if shift >= 64 {
				return ErrIntOverflowLicense
			}
			if iNdEx >= l {
				return io.ErrUnexpectedEOF
			}
			b := dAtA[iNdEx]
			iNdEx++
			wire |= (uint64(b) & 0x7F) << shift
			if b < 0x80 {
				break
			}
		}
		fieldNum := int32(wire >> 3)
		wireType := int(wire & 0x7)
		if wireType == 4 {
			return fmt.Errorf("proto: License: wiretype end group for non-group")
		}
		if fieldNum <= 0 {
			return fmt.Errorf("proto: License: illegal tag %d (wire type %d)", fieldNum, wire)
		}
		switch fieldNum {
		case 1:
			if wireType != 2 {
				return fmt.Errorf("proto: wrong wireType = %d for field ClusterID", wireType)
			}
			var byteLen int
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return ErrIntOverflowLicense
				}
				if iNdEx >= l {
					return io.ErrUnexpectedEOF
				}
				b := dAtA[iNdEx]
				iNdEx++
				byteLen |= (int(b) & 0x7F) << shift
				if b < 0x80 {
					break
				}
			}
			if byteLen < 0 {
				return ErrInvalidLengthLicense
			}
			postIndex := iNdEx + byteLen
			if postIndex > l {
				return io.ErrUnexpectedEOF
			}
			var v github_com_cockroachdb_cockroach_pkg_util_uuid.UUID
			m.ClusterID = append(m.ClusterID, v)
			if err := m.ClusterID[len(m.ClusterID)-1].Unmarshal(dAtA[iNdEx:postIndex]); err != nil {
				return err
			}
			iNdEx = postIndex
		case 2:
			if wireType != 0 {
				return fmt.Errorf("proto: wrong wireType = %d for field ValidUntilUnixSec", wireType)
			}
			m.ValidUntilUnixSec = 0
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return ErrIntOverflowLicense
				}
				if iNdEx >= l {
					return io.ErrUnexpectedEOF
				}
				b := dAtA[iNdEx]
				iNdEx++
				m.ValidUntilUnixSec |= (int64(b) & 0x7F) << shift
				if b < 0x80 {
					break
				}
			}
		case 3:
			if wireType != 0 {
				return fmt.Errorf("proto: wrong wireType = %d for field Type", wireType)
			}
			m.Type = 0
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return ErrIntOverflowLicense
				}
				if iNdEx >= l {
					return io.ErrUnexpectedEOF
				}
				b := dAtA[iNdEx]
				iNdEx++
				m.Type |= (License_Type(b) & 0x7F) << shift
				if b < 0x80 {
					break
				}
			}
		case 4:
			if wireType != 2 {
				return fmt.Errorf("proto: wrong wireType = %d for field OrganizationName", wireType)
			}
			var stringLen uint64
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return ErrIntOverflowLicense
				}
				if iNdEx >= l {
					return io.ErrUnexpectedEOF
				}
				b := dAtA[iNdEx]
				iNdEx++
				stringLen |= (uint64(b) & 0x7F) << shift
				if b < 0x80 {
					break
				}
			}
			intStringLen := int(stringLen)
			if intStringLen < 0 {
				return ErrInvalidLengthLicense
			}
			postIndex := iNdEx + intStringLen
			if postIndex > l {
				return io.ErrUnexpectedEOF
			}
			m.OrganizationName = string(dAtA[iNdEx:postIndex])
			iNdEx = postIndex
		default:
			iNdEx = preIndex
			skippy, err := skipLicense(dAtA[iNdEx:])
			if err != nil {
				return err
			}
			if skippy < 0 {
				return ErrInvalidLengthLicense
			}
			if (iNdEx + skippy) > l {
				return io.ErrUnexpectedEOF
			}
			iNdEx += skippy
		}
	}

	if iNdEx > l {
		return io.ErrUnexpectedEOF
	}
	return nil
}
func skipLicense(dAtA []byte) (n int, err error) {
	l := len(dAtA)
	iNdEx := 0
	for iNdEx < l {
		var wire uint64
		for shift := uint(0); ; shift += 7 {
			if shift >= 64 {
				return 0, ErrIntOverflowLicense
			}
			if iNdEx >= l {
				return 0, io.ErrUnexpectedEOF
			}
			b := dAtA[iNdEx]
			iNdEx++
			wire |= (uint64(b) & 0x7F) << shift
			if b < 0x80 {
				break
			}
		}
		wireType := int(wire & 0x7)
		switch wireType {
		case 0:
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return 0, ErrIntOverflowLicense
				}
				if iNdEx >= l {
					return 0, io.ErrUnexpectedEOF
				}
				iNdEx++
				if dAtA[iNdEx-1] < 0x80 {
					break
				}
			}
			return iNdEx, nil
		case 1:
			iNdEx += 8
			return iNdEx, nil
		case 2:
			var length int
			for shift := uint(0); ; shift += 7 {
				if shift >= 64 {
					return 0, ErrIntOverflowLicense
				}
				if iNdEx >= l {
					return 0, io.ErrUnexpectedEOF
				}
				b := dAtA[iNdEx]
				iNdEx++
				length |= (int(b) & 0x7F) << shift
				if b < 0x80 {
					break
				}
			}
			iNdEx += length
			if length < 0 {
				return 0, ErrInvalidLengthLicense
			}
			return iNdEx, nil
		case 3:
			for {
				var innerWire uint64
				var start int = iNdEx
				for shift := uint(0); ; shift += 7 {
					if shift >= 64 {
						return 0, ErrIntOverflowLicense
					}
					if iNdEx >= l {
						return 0, io.ErrUnexpectedEOF
					}
					b := dAtA[iNdEx]
					iNdEx++
					innerWire |= (uint64(b) & 0x7F) << shift
					if b < 0x80 {
						break
					}
				}
				innerWireType := int(innerWire & 0x7)
				if innerWireType == 4 {
					break
				}
				next, err := skipLicense(dAtA[start:])
				if err != nil {
					return 0, err
				}
				iNdEx = start + next
			}
			return iNdEx, nil
		case 4:
			return iNdEx, nil
		case 5:
			iNdEx += 4
			return iNdEx, nil
		default:
			return 0, fmt.Errorf("proto: illegal wireType %d", wireType)
		}
	}
	panic("unreachable")
}

var (
	ErrInvalidLengthLicense = fmt.Errorf("proto: negative length found during unmarshaling")
	ErrIntOverflowLicense   = fmt.Errorf("proto: integer overflow")
)

func init() { proto.RegisterFile("cockroach/pkg/ccl/utilccl/license.proto", fileDescriptorLicense) }

var fileDescriptorLicense = []byte{
	// 358 bytes of a gzipped FileDescriptorProto
	0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02, 0xff, 0x6c, 0x90, 0x31, 0x8b, 0xdb, 0x30,
	0x1c, 0xc5, 0xad, 0x24, 0x34, 0x44, 0xb4, 0xc1, 0x31, 0x29, 0x98, 0x0e, 0x8e, 0x9b, 0x0e, 0x35,
	0x14, 0x24, 0x68, 0x86, 0xd2, 0x35, 0x49, 0xa1, 0x81, 0x92, 0xc1, 0xad, 0x97, 0x2e, 0x46, 0x91,
	0x85, 0x23, 0x22, 0x4b, 0xc6, 0x96, 0x42, 0xd2, 0x4f, 0xd1, 0x0f, 0xd5, 0x21, 0x63, 0xc7, 0xe3,
	0x86, 0x70, 0xe7, 0xfb, 0x22, 0x87, 0x1d, 0x13, 0xee, 0xe0, 0x26, 0x49, 0xbc, 0xf7, 0x7b, 0xfa,
	0xbf, 0x3f, 0xfc, 0x48, 0x15, 0xdd, 0x15, 0x8a, 0xd0, 0x2d, 0xce, 0x77, 0x29, 0xa6, 0x54, 0x60,
	0xa3, 0xb9, 0xa8, 0x4f, 0xc1, 0x29, 0x93, 0x25, 0x43, 0x79, 0xa1, 0xb4, 0x72, 0xde, 0x5e, 0x8d,
	0x88, 0x52, 0x81, 0x5a, 0xd3, 0xbb, 0x71, 0xaa, 0x52, 0xd5, 0x38, 0x70, 0x7d, 0xbb, 0x98, 0xa7,
	0xff, 0x3a, 0xb0, 0xff, 0xe3, 0x82, 0x3b, 0x29, 0x84, 0x54, 0x98, 0x52, 0xb3, 0x22, 0xe6, 0x89,
	0x0b, 0xfc, 0x6e, 0xf0, 0x7a, 0xfe, 0xfd, 0x74, 0x9e, 0x58, 0xb7, 0xe7, 0xc9, 0x2c, 0xe5, 0x7a,
	0x6b, 0x36, 0x88, 0xaa, 0x0c, 0x5f, 0xf3, 0x93, 0x0d, 0x7e, 0x3e, 0x54, 0xfd, 0x17, 0x36, 0x86,
	0x27, 0x28, 0x8a, 0x56, 0xcb, 0xea, 0x3c, 0x19, 0x2c, 0x2e, 0x81, 0xab, 0x65, 0x38, 0x68, 0xb3,
	0x57, 0x89, 0x83, 0xe1, 0x78, 0x4f, 0x04, 0x4f, 0x62, 0x23, 0x35, 0x17, 0xb1, 0x91, 0xfc, 0x10,
	0x97, 0x8c, 0xba, 0x1d, 0x1f, 0x04, 0xdd, 0x70, 0xd4, 0x68, 0x51, 0x2d, 0x45, 0x92, 0x1f, 0x7e,
	0x32, 0xea, 0x7c, 0x81, 0x3d, 0x7d, 0xcc, 0x99, 0xdb, 0xf5, 0x41, 0x30, 0xfc, 0xfc, 0x01, 0xbd,
	0xd8, 0x10, 0xb5, 0x3d, 0xd0, 0xaf, 0x63, 0xce, 0xc2, 0x06, 0x70, 0x3e, 0xc1, 0x91, 0x2a, 0x52,
	0x22, 0xf9, 0x1f, 0xa2, 0xb9, 0x92, 0xb1, 0x24, 0x19, 0x73, 0x7b, 0x3e, 0x08, 0x06, 0xa1, 0xfd,
	0x54, 0x58, 0x93, 0x8c, 0x4d, 0xbf, 0xc2, 0x5e, 0x8d, 0x3a, 0x23, 0xf8, 0x66, 0xad, 0xe4, 0x42,
	0x65, 0x19, 0x2b, 0x28, 0x27, 0xc2, 0xb6, 0x9c, 0x21, 0x84, 0xdf, 0xa4, 0x66, 0x45, 0x5e, 0xf0,
	0x92, 0xd9, 0xa0, 0x79, 0xef, 0x89, 0x30, 0x0d, 0x6c, 0x77, 0xe6, 0xef, 0x4f, 0xf7, 0x9e, 0x75,
	0xaa, 0x3c, 0xf0, 0xbf, 0xf2, 0xc0, 0x4d, 0xe5, 0x81, 0xbb, 0xca, 0x03, 0x7f, 0x1f, 0x3c, 0xeb,
	0x77, 0xbf, 0x9d, 0x6e, 0xf3, 0xaa, 0x59, 0xf8, 0xec, 0x31, 0x00, 0x00, 0xff, 0xff, 0x19, 0xec,
	0x22, 0xa1, 0xc8, 0x01, 0x00, 0x00,
}
