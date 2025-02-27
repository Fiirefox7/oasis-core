//! Consensus SGX and quote policy handling.

use std::sync::Arc;

use anyhow::{bail, Result};
use io_context::Context;
use slog::{debug, Logger};
use thiserror::Error;

use crate::{
    common::{logger::get_logger, namespace::Namespace, sgx::QuotePolicy, version::Version},
    consensus::{
        keymanager::SignedPolicySGX,
        registry::{SGXConstraints, TEEHardware},
        state::{
            beacon::ImmutableState as BeaconState, keymanager::ImmutableState as KeyManagerState,
            registry::ImmutableState as RegistryState,
        },
        verifier::Verifier,
        HEIGHT_LATEST,
    },
};

/// Policy verifier error.
#[derive(Error, Debug)]
pub enum PolicyVerifierError {
    #[error("missing runtime descriptor")]
    MissingRuntimeDescriptor,
    #[error("no corresponding runtime deployment")]
    NoDeployment,
    #[error("bad TEE constraints")]
    BadTEEConstraints,
    #[error("policy hasn't been published")]
    PolicyNotPublished,
    #[error("configured runtime hardware mismatch")]
    HardwareMismatch,
    #[error("runtime doesn't use key manager")]
    NoKeyManager,
}

/// Consensus policy verifier.
pub struct PolicyVerifier {
    consensus_verifier: Arc<dyn Verifier>,
    logger: Logger,
}

impl PolicyVerifier {
    /// Create a new consensus policy verifier.
    pub fn new(consensus_verifier: Arc<dyn Verifier>) -> Self {
        let logger = get_logger("runtime/policy_verifier");
        Self {
            consensus_verifier,
            logger,
        }
    }

    /// Fetch runtime's quote policy from the latest verified consensus layer state.
    ///
    /// If the runtime version is not provided, the policy for the active deployment is returned.
    pub fn quote_policy(
        &self,
        ctx: Arc<Context>,
        runtime_id: &Namespace,
        version: Option<Version>,
        use_latest_state: bool,
    ) -> Result<QuotePolicy> {
        // Verify to the latest height, if needed.
        let consensus_state = if use_latest_state {
            self.consensus_verifier.latest_state()?
        } else {
            self.consensus_verifier.state_at(HEIGHT_LATEST)?
        };

        // Fetch quote policy from the consensus layer using the given or the active version.
        let registry_state = RegistryState::new(&consensus_state);
        let runtime = registry_state
            .runtime(Context::create_child(&ctx), runtime_id)?
            .ok_or(PolicyVerifierError::MissingRuntimeDescriptor)?;

        let ad = match version {
            Some(version) => runtime
                .deployment_for_version(version)
                .ok_or(PolicyVerifierError::NoDeployment)?,
            None => {
                let beacon_state = BeaconState::new(&consensus_state);
                let epoch = beacon_state.epoch(Context::create_child(&ctx))?;

                runtime
                    .active_deployment(epoch)
                    .ok_or(PolicyVerifierError::NoDeployment)?
            }
        };

        let policy = match runtime.tee_hardware {
            TEEHardware::TEEHardwareIntelSGX => {
                let sc: SGXConstraints = ad
                    .try_decode_tee()
                    .map_err(|_| PolicyVerifierError::BadTEEConstraints)?;
                sc.policy()
            }
            _ => bail!(PolicyVerifierError::HardwareMismatch),
        };

        Ok(policy)
    }

    /// Verify that runtime's quote policy has been published in the consensus layer.
    pub fn verify_quote_policy(
        &self,
        ctx: Arc<Context>,
        policy: QuotePolicy,
        runtime_id: &Namespace,
        version: Option<Version>,
        use_latest_state: bool,
    ) -> Result<QuotePolicy> {
        let published_policy = self.quote_policy(ctx, runtime_id, version, use_latest_state)?;

        if policy != published_policy {
            debug!(
                self.logger,
                "quote policy mismatch";
                "untrusted" => ?policy,
                "published" => ?published_policy,
            );
            return Err(PolicyVerifierError::PolicyNotPublished.into());
        }

        Ok(published_policy)
    }

    /// Fetch key manager's policy from the latest verified consensus layer state.
    pub fn key_manager_policy(
        &self,
        ctx: Arc<Context>,
        key_manager: Namespace,
        use_latest_state: bool,
    ) -> Result<SignedPolicySGX> {
        // Verify to the latest height, if needed.
        let consensus_state = if use_latest_state {
            self.consensus_verifier.latest_state()?
        } else {
            self.consensus_verifier.state_at(HEIGHT_LATEST)?
        };

        // Fetch policy from the consensus layer.
        let km_state = KeyManagerState::new(&consensus_state);
        let policy = km_state
            .status(Context::create_child(&ctx), key_manager)?
            .ok_or(PolicyVerifierError::PolicyNotPublished)?
            .policy
            .ok_or(PolicyVerifierError::PolicyNotPublished)?;

        Ok(policy)
    }

    /// Verify that key manager's policy has been published in the consensus layer.
    pub fn verify_key_manager_policy(
        &self,
        ctx: Arc<Context>,
        policy: SignedPolicySGX,
        key_manager: Namespace,
        use_latest_state: bool,
    ) -> Result<SignedPolicySGX> {
        let published_policy = self.key_manager_policy(ctx, key_manager, use_latest_state)?;

        if policy != published_policy {
            debug!(
                self.logger,
                "key manager policy mismatch";
                "untrusted" => ?policy,
                "published" => ?published_policy,
            );
            return Err(PolicyVerifierError::PolicyNotPublished.into());
        }

        Ok(published_policy)
    }

    /// Fetch runtime's key manager.
    pub fn key_manager(
        &self,
        ctx: Arc<Context>,
        runtime_id: &Namespace,
        use_latest_state: bool,
    ) -> Result<Namespace> {
        let consensus_state = if use_latest_state {
            self.consensus_verifier.latest_state()?
        } else {
            self.consensus_verifier.state_at(HEIGHT_LATEST)?
        };

        let registry_state = RegistryState::new(&consensus_state);
        let runtime = registry_state
            .runtime(Context::create_child(&ctx), runtime_id)?
            .ok_or(PolicyVerifierError::MissingRuntimeDescriptor)?;
        let key_manager = runtime
            .key_manager
            .ok_or(PolicyVerifierError::NoKeyManager)?;

        Ok(key_manager)
    }
}
