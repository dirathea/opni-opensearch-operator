package opensearch

import (
	"bytes"
	"errors"
	"fmt"
	"text/template"

	"github.com/rancher/opni-opensearch-operator/pkg/resources"
	"github.com/rancher/opni/pkg/util"
	"golang.org/x/crypto/bcrypt"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	passwordSecretName      = "opni-es-password"
	internalUsersSecretName = "opni-es-internalusers"
	internalUsersKey        = "internal_users.yml"
	bcryptCost              = 5
)

var (
	defaultLoggingConfig = `appender:
  console:
    layout:
      conversionPattern: '[%d{ISO8601}][%-5p][%-25c] %m%n'
      type: consolePattern
    type: console
es.logger.level: INFO
logger:
  action: DEBUG
  com.amazonaws: WARN
rootLogger: ${es.logger.level}, console`

	internalUsersTemplate = template.Must(template.New("internalusers").Parse(`
_meta:
  type: "internalusers"
  config_version: 2
admin:
  hash: "{{ . }}"
  reserved: true
  backend_roles:
  - "admin"
  description: "Admin user"
kibanaserver:
  hash: "$2a$12$4AcgAt3xwOWadA5s5blL6ev39OXDNhmOesEoo33eZtrq2N0YrU3H."
  reserved: true
  description: "Kibana server user"
`))
)

func (r *Reconciler) opensearchConfigSecret() resources.Resource {
	secretName := "opni-es-config"
	if r.opensearchCluster.Spec.ConfigSecret != nil {
		// If auth secret is provided, use it instead of creating a new one. If
		// the secret doesn't exist, create one with the given name.
		secretName = r.opensearchCluster.Spec.ConfigSecret.Name
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: r.opensearchCluster.Namespace,
		},
		StringData: map[string]string{
			"logging.yml": defaultLoggingConfig,
		},
	}

	ctrl.SetControllerReference(r.opensearchCluster, secret, r.client.Scheme())
	return resources.Present(secret)
}

func (r *Reconciler) opensearchPasswordResourcces() (err error) {
	var password []byte
	var hash []byte
	var buffer bytes.Buffer
	var passwordSecretRef *corev1.SecretKeySelector
	var ok bool

	lg := log.FromContext(r.ctx)
	generatePassword := r.opensearchCluster.Status.Auth.GenerateOpensearchHash == nil || *r.opensearchCluster.Status.Auth.GenerateOpensearchHash

	// Fetch or create the password secret
	if r.opensearchCluster.Spec.AdminPasswordFrom != nil {
		passwordSecretRef = r.opensearchCluster.Spec.AdminPasswordFrom
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      r.opensearchCluster.Spec.AdminPasswordFrom.Name,
				Namespace: r.opensearchCluster.Namespace,
			},
		}
		err = r.client.Get(r.ctx, client.ObjectKeyFromObject(secret), secret)
		if err != nil {
			return
		}
		password, ok = secret.Data[r.opensearchCluster.Spec.AdminPasswordFrom.Key]
		if !ok {
			return fmt.Errorf("%s key does not exist in %s", r.opensearchCluster.Spec.AdminPasswordFrom.Key, r.opensearchCluster.Spec.AdminPasswordFrom.Name)
		}

	} else {
		if generatePassword {
			password = util.GenerateRandomPassword()
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      passwordSecretName,
					Namespace: r.opensearchCluster.Namespace,
				},
				Data: map[string][]byte{
					"password": password,
				},
			}
			ctrl.SetControllerReference(r.opensearchCluster, secret, r.client.Scheme())
			err = util.CreateOrUpdate(r.ctx, r.client, secret)
			if err != nil {
				return err
			}

		} else {
			// fetch the existing secret
			existingSecret := corev1.Secret{}
			err := r.client.Get(r.ctx, types.NamespacedName{
				Name:      passwordSecretName,
				Namespace: r.opensearchCluster.Namespace,
			}, &existingSecret)

			// If we can't get the secret return an error
			if k8serrors.IsNotFound(err) {
				retry.RetryOnConflict(retry.DefaultRetry, func() error {
					if err := r.client.Get(r.ctx, client.ObjectKeyFromObject(r.opensearchCluster), r.opensearchCluster); err != nil {
						return err
					}
					r.opensearchCluster.Status.Auth.GenerateOpensearchHash = pointer.BoolPtr(true)
					return r.client.Status().Update(r.ctx, r.opensearchCluster)
				})
				return errors.New("password secret not found, will recreate on next loop")
			} else if err != nil {
				lg.Error(err, "failed to check password secret")
				return err
			}
		}

		passwordSecretRef = &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{
				Name: passwordSecretName,
			},
			Key: "password",
		}
	}

	// Generate the internal_users secret
	if generatePassword {
		lg.V(1).Info("generating bcrypt hash of password; this is slow")
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      internalUsersSecretName,
				Namespace: r.opensearchCluster.Namespace,
			},
			Data: map[string][]byte{},
		}

		// Check the namespace for test annotation
		ns := corev1.Namespace{}
		r.client.Get(r.ctx, types.NamespacedName{
			Name: r.opensearchCluster.Namespace,
		}, &ns)

		if value, ok := ns.Annotations["controller-test"]; ok && value == "true" {
			lg.V(1).Info("test namespace, using minimum bcrypt difficulty to hash password")
			hash, err = bcrypt.GenerateFromPassword(password, 4)
		} else {
			hash, err = bcrypt.GenerateFromPassword(password, bcryptCost)
		}
		if err != nil {
			return
		}

		err = internalUsersTemplate.Execute(&buffer, string(hash))
		if err != nil {
			return
		}

		secret.Data[internalUsersKey] = buffer.Bytes()
		ctrl.SetControllerReference(r.opensearchCluster, secret, r.client.Scheme())
		err = util.CreateOrUpdate(r.ctx, r.client, secret)
		if err != nil {
			return
		}
	}

	// Update the status with the password ref
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.client.Get(r.ctx, client.ObjectKeyFromObject(r.opensearchCluster), r.opensearchCluster); err != nil {
			return err
		}
		r.opensearchCluster.Status.Auth.OpensearchAuthSecretKeyRef = passwordSecretRef
		r.opensearchCluster.Status.Auth.GenerateOpensearchHash = pointer.BoolPtr(false)
		return r.client.Status().Update(r.ctx, r.opensearchCluster)
	})
	return
}