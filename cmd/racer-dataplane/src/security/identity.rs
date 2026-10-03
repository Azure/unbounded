//! Application compatibility path for the independently owned identity component.
pub use racer_identity::{
    Certificates, KeyEpochs, KeyLease, KeyPurpose, Keyring, PendingIdentity, SigningIdentity,
    VerifiedPeer, canonical_uuid,
};

// Compatibility names for existing application fixtures; these are app-local.
#[cfg(test)]
pub(crate) use super::fixtures as tests;
#[cfg(test)]
pub(crate) use super::fixtures as keyring_tests;

impl From<racer_identity::Error> for crate::error::Error {
    fn from(error: racer_identity::Error) -> Self {
        match error {
            racer_identity::Error::InvalidRequest => Self::InvalidRequest,
            racer_identity::Error::InvalidConfiguration => Self::InvalidConfiguration,
            racer_identity::Error::Unauthorized => Self::Unauthorized,
            racer_identity::Error::Unavailable => Self::Unavailable,
            racer_identity::Error::MissingKey => Self::MissingKey,
            racer_identity::Error::CorruptRecord => Self::CorruptRecord,
        }
    }
}

#[cfg(test)]
mod error_tests {
    #[test]
    fn complete_component_error_mapping_preserves_application_meanings() {
        use crate::error::Error as App;
        use racer_identity::Error as Identity;
        for (component, application) in [
            (Identity::InvalidRequest, App::InvalidRequest),
            (Identity::InvalidConfiguration, App::InvalidConfiguration),
            (Identity::Unauthorized, App::Unauthorized),
            (Identity::Unavailable, App::Unavailable),
            (Identity::MissingKey, App::MissingKey),
            (Identity::CorruptRecord, App::CorruptRecord),
        ] {
            assert_eq!(App::from(component), application);
        }
    }
}
