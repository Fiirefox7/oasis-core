// Package remote provides a gRPC backed signer (both client and server).
package remote

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"

	"google.golang.org/grpc"

	"github.com/oasisprotocol/oasis-core/go/common/crypto/signature"
	cmnGrpc "github.com/oasisprotocol/oasis-core/go/common/grpc"
)

// SignerName is the name used to identify the remote signer.
const SignerName = "remote"

var (
	serviceName = cmnGrpc.NewServiceName("RemoteSigner")

	methodPublicKeys = serviceName.NewMethod("PublicKeys", nil)
	methodSign       = serviceName.NewMethod("Sign", SignRequest{})

	serviceDesc = grpc.ServiceDesc{
		ServiceName: string(serviceName),
		HandlerType: (*Backend)(nil),
		Methods: []grpc.MethodDesc{
			{
				MethodName: methodPublicKeys.ShortName(),
				Handler:    handlerPublicKeys,
			},
			{
				MethodName: methodSign.ShortName(),
				Handler:    handlerSign,
			},
		},
	}
)

// PublicKey is a public key supported by the remote signer.
type PublicKey struct {
	Role      signature.SignerRole `json:"role"`
	PublicKey signature.PublicKey  `json:"public_key"`
}

// SignRequest is a signature request.
type SignRequest struct {
	Role    signature.SignerRole `json:"role"`
	Context string               `json:"context"`
	Message []byte               `json:"message"`
}

// Backend is the remote signer backend interface.
type Backend interface {
	PublicKeys(context.Context) ([]PublicKey, error)
	Sign(context.Context, *SignRequest) ([]byte, error)
}

type wrapper struct {
	signers map[signature.SignerRole]signature.Signer
}

func (w *wrapper) PublicKeys(ctx context.Context) ([]PublicKey, error) {
	var resp []PublicKey
	for _, v := range signature.SignerRoles { // Return in consistent order.
		if signer := w.signers[v]; signer != nil {
			resp = append(resp, PublicKey{
				Role:      v,
				PublicKey: signer.Public(),
			})
		}
	}
	return resp, nil
}

func (w *wrapper) Sign(ctx context.Context, req *SignRequest) ([]byte, error) {
	signer, ok := w.signers[req.Role]
	if !ok {
		return nil, signature.ErrNotExist
	}
	return signer.ContextSign(signature.Context(req.Context), req.Message)
}

func handlerPublicKeys( // nolint: golint
	srv interface{},
	ctx context.Context,
	dec func(interface{}) error,
	interceptor grpc.UnaryServerInterceptor,
) (interface{}, error) {
	if interceptor == nil {
		return srv.(Backend).PublicKeys(ctx)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: methodPublicKeys.FullName(),
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(Backend).PublicKeys(ctx)
	}
	return interceptor(ctx, nil, info, handler)
}

func handlerSign( // nolint: golint
	srv interface{},
	ctx context.Context,
	dec func(interface{}) error,
	interceptor grpc.UnaryServerInterceptor,
) (interface{}, error) {
	var req SignRequest
	if err := dec(&req); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(Backend).Sign(ctx, &req)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: methodSign.FullName(),
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(Backend).Sign(ctx, req.(*SignRequest))
	}
	return interceptor(ctx, &req, info, handler)
}

// RegisterService registers a new remote signer backend service with the given
// gRPC server.
func RegisterService(server *grpc.Server, signerFactory signature.SignerFactory) {
	if !signature.IsUnsafeUnregisteredContextsAllowed() {
		panic("signature/signer/remote: context registration bypass is required")
	}

	// Load all signers, ignoring errors.
	w := &wrapper{
		signers: make(map[signature.SignerRole]signature.Signer),
	}
	for _, v := range signature.SignerRoles {
		signer, err := signerFactory.Load(v)
		if err == nil {
			w.signers[v] = signer
		}
	}

	server.RegisterService(&serviceDesc, w)
}

type remoteFactory struct {
	conn   *grpc.ClientConn
	reqCtx context.Context

	signers map[signature.SignerRole]*remoteSigner
}

func (rf *remoteFactory) EnsureRole(role signature.SignerRole) error {
	if rf.signers[role] == nil {
		return signature.ErrNotExist
	}
	return nil
}

func (rf *remoteFactory) Generate(role signature.SignerRole, rng io.Reader) (signature.Signer, error) {
	return nil, fmt.Errorf("signature/signer/remote: key re-generation prohibited")
}

func (rf *remoteFactory) Load(role signature.SignerRole) (signature.Signer, error) {
	signer := rf.signers[role]
	if signer == nil {
		return nil, signature.ErrNotExist
	}
	return signer, nil
}

type remoteSigner struct {
	factory *remoteFactory

	publicKey signature.PublicKey
	role      signature.SignerRole
}

func (rs *remoteSigner) Public() signature.PublicKey {
	return rs.publicKey
}

func (rs *remoteSigner) ContextSign(context signature.Context, message []byte) ([]byte, error) {
	// Prepare the context (chain separation is done client side).
	rawCtx, err := signature.PrepareSignerContext(context)
	if err != nil {
		return nil, err
	}

	req := &SignRequest{
		Role:    rs.role,
		Context: string(rawCtx),
		Message: message,
	}

	var rsp []byte
	if err := rs.factory.conn.Invoke(rs.factory.reqCtx, methodSign.FullName(), req, &rsp); err != nil {
		return nil, err
	}

	return rsp, nil
}

func (rs *remoteSigner) String() string {
	return "[redacted remote private key]"
}

func (rs *remoteSigner) Reset() {
	// Nothing to do.
}

// FactoryConfig is the remote factory configuration.
type FactoryConfig struct {
	// Address is the remote factory gRPC address.
	Address string
	// ServerCertificate is the server certificate.
	ServerCertificate *tls.Certificate
	// ClientCertificate is the client certificate.
	ClientCertificate *tls.Certificate
}

// NewFactory creates a new factory with the specified roles.
func NewFactory(config interface{}, roles ...signature.SignerRole) (signature.SignerFactory, error) {
	cfg, ok := config.(*FactoryConfig)
	if !ok {
		return nil, fmt.Errorf("signature/signer/remote: invalid remote signer configuration provided")
	}

	if cfg.ServerCertificate == nil {
		return nil, fmt.Errorf("signature/signer/remote: server certificate is required")
	}

	serverCert, err := x509.ParseCertificate(cfg.ServerCertificate.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("signature/signer/remote: failed to parse server certificate: %w", err)
	}

	creds, err := cmnGrpc.NewClientCreds(&cmnGrpc.ClientOptions{
		Certificates: []tls.Certificate{
			*cfg.ClientCertificate,
		},
		GetServerPubKeys: cmnGrpc.ServerPubKeysGetterFromCertificate(serverCert),
		CommonName:       "remote-signer-server",
	})
	if err != nil {
		return nil, err
	}

	conn, err := cmnGrpc.Dial(cfg.Address, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("signature/signer/remote: failed to dial server: %w", err)
	}

	return NewRemoteFactory(context.Background(), conn)
}

// NewRemoteFactory creates a new gRPC remote signer client service given an
// existing grpc connection.
func NewRemoteFactory(ctx context.Context, conn *grpc.ClientConn) (signature.SignerFactory, error) {
	// Enumerate the keys available, and cache them.
	var rsp []PublicKey
	if err := conn.Invoke(ctx, methodPublicKeys.FullName(), nil, &rsp); err != nil {
		return nil, err
	}

	rf := &remoteFactory{
		conn:    conn,
		reqCtx:  ctx,
		signers: make(map[signature.SignerRole]*remoteSigner),
	}
	for _, v := range rsp {
		rf.signers[v.Role] = &remoteSigner{
			factory:   rf,
			publicKey: v.PublicKey,
			role:      v.Role,
		}
	}

	return rf, nil
}
