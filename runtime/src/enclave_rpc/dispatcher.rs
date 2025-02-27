//! RPC dispatcher.
use std::collections::HashMap;

use anyhow::{bail, Result};
use thiserror::Error;

use crate::{common::sgx::QuotePolicy, consensus::keymanager::SignedPolicySGX};

use super::{
    context::Context,
    types::{Body, Kind, Request, Response},
};

/// Dispatch error.
#[derive(Error, Debug)]
enum DispatchError {
    #[error("method not found: {method:?}")]
    MethodNotFound { method: String },
    #[error("invalid RPC kind: {method:?} ({kind:?})")]
    InvalidRpcKind { method: String, kind: Kind },
}

/// Custom context initializer.
pub trait ContextInitializer {
    /// Called to initialize the context.
    fn init(&self, ctx: &mut Context);
}

impl<F> ContextInitializer for F
where
    F: Fn(&mut Context),
{
    fn init(&self, ctx: &mut Context) {
        (*self)(ctx)
    }
}

/// Descriptor of a RPC API method.
#[derive(Clone, Debug)]
pub struct MethodDescriptor {
    /// Method name.
    pub name: String,
    /// Specifies which kind of RPC is allowed to call the method.
    pub kind: Kind,
}

/// Handler for a RPC method.
pub trait MethodHandler<Rq, Rsp> {
    /// Invoke the method implementation and return a response.
    fn handle(&self, request: &Rq, ctx: &mut Context) -> Result<Rsp>;
}

impl<Rq, Rsp, F> MethodHandler<Rq, Rsp> for F
where
    Rq: 'static,
    Rsp: 'static,
    F: Fn(&Rq, &mut Context) -> Result<Rsp> + 'static,
{
    fn handle(&self, request: &Rq, ctx: &mut Context) -> Result<Rsp> {
        (*self)(request, ctx)
    }
}

/// Dispatcher for a RPC method.
pub trait MethodHandlerDispatch {
    /// Get method descriptor.
    fn get_descriptor(&self) -> &MethodDescriptor;

    /// Dispatch request.
    fn dispatch(&self, request: Request, ctx: &mut Context) -> Result<Response>;
}

struct MethodHandlerDispatchImpl<Rq, Rsp> {
    /// Method descriptor.
    descriptor: MethodDescriptor,
    /// Method handler.
    handler: Box<dyn MethodHandler<Rq, Rsp> + Send + Sync>,
}

impl<Rq, Rsp> MethodHandlerDispatch for MethodHandlerDispatchImpl<Rq, Rsp>
where
    Rq: cbor::Decode + 'static,
    Rsp: cbor::Encode + 'static,
{
    fn get_descriptor(&self) -> &MethodDescriptor {
        &self.descriptor
    }

    fn dispatch(&self, request: Request, ctx: &mut Context) -> Result<Response> {
        let request = cbor::from_value(request.args)?;
        let response = self.handler.handle(&request, ctx)?;

        Ok(Response {
            body: Body::Success(cbor::to_value(response)),
        })
    }
}

/// RPC method dispatcher implementation.
pub struct Method {
    /// Method dispatcher.
    dispatcher: Box<dyn MethodHandlerDispatch + Send + Sync>,
}

impl Method {
    /// Create a new enclave method descriptor.
    pub fn new<Rq, Rsp, Handler>(method: MethodDescriptor, handler: Handler) -> Self
    where
        Rq: cbor::Decode + 'static,
        Rsp: cbor::Encode + 'static,
        Handler: MethodHandler<Rq, Rsp> + Send + Sync + 'static,
    {
        Method {
            dispatcher: Box::new(MethodHandlerDispatchImpl {
                descriptor: method,
                handler: Box::new(handler),
            }),
        }
    }

    /// Return method name.
    fn get_name(&self) -> &String {
        &self.dispatcher.get_descriptor().name
    }

    /// Return RPC call kind.
    fn get_kind(&self) -> Kind {
        self.dispatcher.get_descriptor().kind
    }

    /// Dispatch a request.
    fn dispatch(&self, request: Request, ctx: &mut Context) -> Result<Response> {
        self.dispatcher.dispatch(request, ctx)
    }
}

/// Key manager policy update handler callback.
pub type KeyManagerPolicyHandler = dyn Fn(SignedPolicySGX) + Send + Sync;
/// Key manager quote policy update handler callback.
pub type KeyManagerQuotePolicyHandler = dyn Fn(QuotePolicy) + Send + Sync;

/// RPC call dispatcher.
#[derive(Default)]
pub struct Dispatcher {
    /// Registered RPC methods.
    methods: HashMap<String, Method>,
    /// Registered key manager policy handler.
    km_policy_handler: Option<Box<KeyManagerPolicyHandler>>,
    /// Registered key manager quote policy handler.
    km_quote_policy_handler: Option<Box<KeyManagerQuotePolicyHandler>>,
    /// Registered context initializer.
    ctx_initializer: Option<Box<dyn ContextInitializer + Send + Sync>>,
}

impl Dispatcher {
    /// Register a new method in the dispatcher.
    pub fn add_method(&mut self, method: Method) {
        self.methods.insert(method.get_name().clone(), method);
    }

    /// Configure context initializer.
    pub fn set_context_initializer<I>(&mut self, initializer: I)
    where
        I: ContextInitializer + Send + Sync + 'static,
    {
        self.ctx_initializer = Some(Box::new(initializer));
    }

    /// Dispatch request.
    pub fn dispatch(&self, mut ctx: Context, request: Request, kind: Kind) -> Response {
        if let Some(ref ctx_init) = self.ctx_initializer {
            ctx_init.init(&mut ctx);
        }

        match self.dispatch_fallible(&mut ctx, request, kind) {
            Ok(response) => response,
            Err(error) => Response {
                body: Body::Error(format!("{}", error)),
            },
        }
    }

    fn dispatch_fallible(
        &self,
        ctx: &mut Context,
        request: Request,
        kind: Kind,
    ) -> Result<Response> {
        let method = match self.methods.get(&request.method) {
            Some(method) => method,
            None => bail!(DispatchError::MethodNotFound {
                method: request.method,
            }),
        };

        match (method.get_kind(), kind) {
            (Kind::NoiseSession, Kind::NoiseSession) => {}
            (Kind::InsecureQuery, Kind::InsecureQuery) => {}
            (Kind::InsecureQuery, Kind::NoiseSession) => {}
            (Kind::LocalQuery, Kind::LocalQuery) => {}
            _ => bail!(DispatchError::InvalidRpcKind {
                method: request.method,
                kind,
            }),
        };

        method.dispatch(request, ctx)
    }

    /// Handle key manager policy update.
    pub fn handle_km_policy_update(&self, policy: SignedPolicySGX) {
        if let Some(handler) = self.km_policy_handler.as_ref() {
            handler(policy)
        }
    }

    /// Handle key manager quote policy update.
    pub fn handle_km_quote_policy_update(&self, policy: QuotePolicy) {
        if let Some(handler) = self.km_quote_policy_handler.as_ref() {
            handler(policy)
        }
    }

    /// Update key manager policy update handler.
    pub fn set_keymanager_policy_update_handler(
        &mut self,
        f: Option<Box<KeyManagerPolicyHandler>>,
    ) {
        self.km_policy_handler = f;
    }

    /// Update key manager quote policy update handler.
    pub fn set_keymanager_quote_policy_update_handler(
        &mut self,
        f: Option<Box<KeyManagerQuotePolicyHandler>>,
    ) {
        self.km_quote_policy_handler = f;
    }
}
