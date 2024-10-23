/*
Copyright The CloudNativePG Contributors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"fmt"
	"os"
	"strings"
	"time"

	volumesnapshot "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	"github.com/thoas/go-funk"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/reconciler/replicaclusterswitch"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/utils"
	"github.com/cloudnative-pg/cloudnative-pg/tests"
	testUtils "github.com/cloudnative-pg/cloudnative-pg/tests/utils"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Replica Mode", Label(tests.LabelReplication), func() {
	const (
		replicaModeClusterDir = "/replica_mode_cluster/"
		srcClusterName        = "cluster-replica-src"
		srcClusterSample      = fixturesDir + replicaModeClusterDir + srcClusterName + ".yaml.template"
		level                 = tests.Medium
	)

	// those values are present in the cluster manifests
	const (
		// sourceDBName is the name of the database in the source cluster
		sourceDBName = "appSrc"
		// Application database configuration is skipped for replica clusters,
		// so we expect these to not be present
		replicaDBName = "appTgt"
		replicaUser   = "userTgt"
	)

	BeforeEach(func() {
		if testLevelEnv.Depth < int(level) {
			Skip("Test depth is lower than the amount requested for this test")
		}
	})

	Context("can bootstrap a replica cluster using TLS auth", func() {
		It("should work", func() {
			const (
				replicaClusterSampleTLS = fixturesDir + replicaModeClusterDir + "cluster-replica-tls.yaml.template"
				replicaNamespacePrefix  = "replica-mode-tls-auth"
				testTableName           = "replica_mode_tls_auth"
			)

			replicaNamespace, err := env.CreateUniqueTestNamespace(replicaNamespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			AssertCreateCluster(replicaNamespace, srcClusterName, srcClusterSample, env)

			AssertReplicaModeCluster(
				replicaNamespace,
				srcClusterName,
				sourceDBName,
				replicaClusterSampleTLS,
				testTableName,
			)

			replicaName, err := env.GetResourceNameFromYAML(replicaClusterSampleTLS)
			Expect(err).ToNot(HaveOccurred())

			assertReplicaClusterTopology(replicaNamespace, replicaName)

			AssertSwitchoverOnReplica(replicaNamespace, replicaName, env)

			assertReplicaClusterTopology(replicaNamespace, replicaName)
		})
	})

	Context("can bootstrap a replica cluster using basic auth", func() {
		It("can be detached from the source cluster", func() {
			const (
				replicaClusterSampleBasicAuth = fixturesDir + replicaModeClusterDir + "cluster-replica-basicauth.yaml.template"
				replicaNamespacePrefix        = "replica-mode-basic-auth"
				testTableName                 = "replica_mode_basic_auth"
			)

			replicaClusterName, err := env.GetResourceNameFromYAML(replicaClusterSampleBasicAuth)
			Expect(err).ToNot(HaveOccurred())
			replicaNamespace, err := env.CreateUniqueTestNamespace(replicaNamespacePrefix)
			Expect(err).ToNot(HaveOccurred())
			AssertCreateCluster(replicaNamespace, srcClusterName, srcClusterSample, env)

			AssertReplicaModeCluster(
				replicaNamespace,
				srcClusterName,
				sourceDBName,
				replicaClusterSampleBasicAuth,
				testTableName,
			)

			AssertDetachReplicaModeCluster(
				replicaNamespace,
				srcClusterName,
				sourceDBName,
				replicaClusterName,
				replicaDBName,
				replicaUser,
				"replica_mode_basic_auth_detach")
		})

		It("should be able to switch to replica cluster and sync data", func(ctx SpecContext) {
			const (
				clusterOneName = "cluster-one"
				clusterTwoName = "cluster-two"
				clusterOneFile = fixturesDir + replicaModeClusterDir +
					"cluster-demotion-one.yaml.template"
				clusterTwoFile = fixturesDir + replicaModeClusterDir +
					"cluster-demotion-two.yaml.template"
				testTableName = "replica_promotion_demotion"
			)
			var clusterOnePrimary, clusterTwoPrimary *corev1.Pod

			getReplicaClusterSwitchCondition := func(conditions []metav1.Condition) *metav1.Condition {
				for _, condition := range conditions {
					if condition.Type == replicaclusterswitch.ConditionReplicaClusterSwitch {
						return &condition
					}
				}
				return nil
			}

			namespace, err := env.CreateUniqueTestNamespace("replica-promotion-demotion")
			Expect(err).ToNot(HaveOccurred())
			AssertCreateCluster(namespace, clusterOneName, clusterOneFile, env)

			AssertReplicaModeCluster(
				namespace,
				clusterOneName,
				sourceDBName,
				clusterTwoFile,
				testTableName,
			)

			// turn the src cluster into a replica
			By("setting replica mode on the src cluster", func() {
				cluster, err := env.GetCluster(namespace, clusterOneName)
				Expect(err).ToNot(HaveOccurred())
				updateTime := time.Now().Truncate(time.Second)
				cluster.Spec.ReplicaCluster.Enabled = true
				err = env.Client.Update(ctx, cluster)
				Expect(err).ToNot(HaveOccurred())
				Eventually(func(g Gomega) {
					cluster, err := env.GetCluster(namespace, clusterOneName)
					g.Expect(err).ToNot(HaveOccurred())
					condition := getReplicaClusterSwitchCondition(cluster.Status.Conditions)
					g.Expect(condition).ToNot(BeNil())
					g.Expect(condition.Status).To(Equal(metav1.ConditionTrue))
					g.Expect(condition.LastTransitionTime.Time).To(BeTemporally(">=", updateTime))
				}).WithTimeout(30 * time.Second).Should(Succeed())
				AssertClusterIsReady(namespace, clusterOneName, testTimeouts[testUtils.ClusterIsReady], env)
			})

			By("checking that src cluster is now a replica cluster", func() {
				Eventually(func() error {
					clusterOnePrimary, err = env.GetClusterPrimary(namespace, clusterOneName)
					return err
				}, 30, 3).Should(BeNil())
				AssertPgRecoveryMode(clusterOnePrimary, true)
			})

			// turn the dst cluster into a primary
			By("disabling the replica mode on the dst cluster", func() {
				cluster, err := env.GetCluster(namespace, clusterTwoName)
				Expect(err).ToNot(HaveOccurred())
				cluster.Spec.ReplicaCluster.Enabled = false
				err = env.Client.Update(ctx, cluster)
				Expect(err).ToNot(HaveOccurred())
				AssertClusterIsReady(namespace, clusterTwoName, testTimeouts[testUtils.ClusterIsReady], env)
			})

			By("checking that dst cluster has been promoted", func() {
				Eventually(func() error {
					clusterTwoPrimary, err = env.GetClusterPrimary(namespace, clusterTwoName)
					return err
				}, 30, 3).Should(BeNil())
				AssertPgRecoveryMode(clusterTwoPrimary, false)
			})

			By("creating a new data in the new source cluster", func() {
				tableLocator := TableLocator{
					Namespace:    namespace,
					ClusterName:  clusterTwoName,
					DatabaseName: sourceDBName,
					TableName:    "new_test_table",
				}
				AssertCreateTestData(env, tableLocator)
			})

			// The dst Cluster gets promoted to primary, hence the new appUser password will
			// be updated to reflect its "-app" secret.
			// We need to copy the password changes over to the src Cluster, which is now a Replica
			// Cluster, in order to connect using the "-app" secret.
			By("updating the appUser secret of the src cluster", func() {
				_, appSecretPassword, err := testUtils.GetCredentials(clusterTwoName, namespace,
					apiv1.ApplicationUserSecretSuffix, env)
				Expect(err).ToNot(HaveOccurred())
				AssertUpdateSecret("password", appSecretPassword, clusterOneName+apiv1.ApplicationUserSecretSuffix,
					namespace, clusterOneName, 30, env)
			})

			By("checking that the data is present in the old src cluster", func() {
				tableLocator := TableLocator{
					Namespace:    namespace,
					ClusterName:  clusterOneName,
					DatabaseName: sourceDBName,
					TableName:    "new_test_table",
				}
				AssertDataExpectedCount(env, tableLocator, 2)
			})
		})
	})

	Context("archive mode set to 'always' on designated primary", func() {
		It("verifies replica cluster can archive WALs from the designated primary", func() {
			const (
				replicaClusterSample   = fixturesDir + replicaModeClusterDir + "cluster-replica-archive-mode-always.yaml.template"
				replicaNamespacePrefix = "replica-mode-archive"
				testTableName          = "replica_mode_archive"
			)

			replicaClusterName, err := env.GetResourceNameFromYAML(replicaClusterSample)
			Expect(err).ToNot(HaveOccurred())
			replicaNamespace, err := env.CreateUniqueTestNamespace(replicaNamespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			By("creating the credentials for minio", func() {
				_, err = testUtils.CreateObjectStorageSecret(
					replicaNamespace,
					"backup-storage-creds",
					"minio",
					"minio123",
					env,
				)
				Expect(err).ToNot(HaveOccurred())
			})

			By("create the certificates for MinIO", func() {
				err := minioEnv.CreateCaSecret(env, replicaNamespace)
				Expect(err).ToNot(HaveOccurred())
			})

			AssertCreateCluster(replicaNamespace, srcClusterName, srcClusterSample, env)

			AssertReplicaModeCluster(
				replicaNamespace,
				srcClusterName,
				sourceDBName,
				replicaClusterSample,
				testTableName,
			)

			// Get primary from replica cluster
			primaryReplicaCluster, err := env.GetClusterPrimary(replicaNamespace, replicaClusterName)
			Expect(err).ToNot(HaveOccurred())

			By("verify archive mode is set to 'always on' designated primary", func() {
				query := "show archive_mode;"
				Eventually(func() (string, error) {
					stdOut, _, err := env.ExecQueryInInstancePod(
						testUtils.PodLocator{
							Namespace: primaryReplicaCluster.Namespace,
							PodName:   primaryReplicaCluster.Name,
						},
						sourceDBName,
						query)
					return strings.Trim(stdOut, "\n"), err
				}, 30).Should(BeEquivalentTo("always"))
			})
			By("verify the WALs are archived from the designated primary", func() {
				// only replica cluster has backup configure to minio,
				// need the server name  be replica cluster name here
				AssertArchiveWalOnMinio(replicaNamespace, srcClusterName, replicaClusterName)
			})
		})
	})

	Context("can bootstrap a replica cluster from a backup", Ordered, func() {
		const (
			clusterSample   = fixturesDir + replicaModeClusterDir + "cluster-replica-src-with-backup.yaml.template"
			namespacePrefix = "replica-cluster-from-backup"
		)
		var namespace, clusterName string

		JustAfterEach(func() {
			if CurrentSpecReport().Failed() {
				env.DumpNamespaceObjects(namespace, "out/"+CurrentSpecReport().LeafNodeText+".log")
			}
		})

		BeforeAll(func() {
			var err error
			namespace, err = env.CreateUniqueTestNamespace(namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			By("creating the credentials for minio", func() {
				_, err = testUtils.CreateObjectStorageSecret(
					namespace,
					"backup-storage-creds",
					"minio",
					"minio123",
					env,
				)
				Expect(err).ToNot(HaveOccurred())
			})

			By("create the certificates for MinIO", func() {
				err := minioEnv.CreateCaSecret(env, namespace)
				Expect(err).ToNot(HaveOccurred())
			})

			// Create the cluster
			clusterName, err = env.GetResourceNameFromYAML(clusterSample)
			Expect(err).ToNot(HaveOccurred())
			AssertCreateCluster(namespace, clusterName, clusterSample, env)
		})

		It("using a Backup from the object store", func() {
			const (
				replicaClusterSample = fixturesDir + replicaModeClusterDir + "cluster-replica-from-backup.yaml.template"
				testTableName        = "replica_mode_backup"
			)

			By("creating a backup and waiting until it's completed", func() {
				backupName := fmt.Sprintf("%v-backup", clusterName)
				backup, err := testUtils.CreateOnDemandBackup(
					namespace,
					clusterName,
					backupName,
					apiv1.BackupTargetStandby,
					apiv1.BackupMethodBarmanObjectStore,
					env)
				Expect(err).ToNot(HaveOccurred())

				Eventually(func() (apiv1.BackupPhase, error) {
					err = env.Client.Get(env.Ctx, types.NamespacedName{
						Namespace: namespace,
						Name:      backupName,
					}, backup)
					return backup.Status.Phase, err
				}, testTimeouts[testUtils.BackupIsReady]).Should(BeEquivalentTo(apiv1.BackupPhaseCompleted))
			})

			By("creating a replica cluster from the backup", func() {
				AssertReplicaModeCluster(
					namespace,
					clusterName,
					sourceDBName,
					replicaClusterSample,
					testTableName,
				)
			})
		})

		It("using a Volume Snapshot", func() {
			const (
				replicaClusterSample = fixturesDir + replicaModeClusterDir + "cluster-replica-from-snapshot.yaml.template"
				snapshotDataEnv      = "REPLICA_CLUSTER_SNAPSHOT_NAME_PGDATA"
				snapshotWalEnv       = "REPLICA_CLUSTER_SNAPSHOT_NAME_PGWAL"
				testTableName        = "replica_mode_snapshot"
			)

			DeferCleanup(func() error {
				err := os.Unsetenv(snapshotDataEnv)
				if err != nil {
					return err
				}
				err = os.Unsetenv(snapshotWalEnv)
				if err != nil {
					return err
				}
				return nil
			})

			var backup *apiv1.Backup
			By("creating a snapshot and waiting until it's completed", func() {
				var err error
				snapshotName := fmt.Sprintf("%v-snapshot", clusterName)
				backup, err = testUtils.CreateOnDemandBackup(
					namespace,
					clusterName,
					snapshotName,
					apiv1.BackupTargetStandby,
					apiv1.BackupMethodVolumeSnapshot,
					env)
				Expect(err).ToNot(HaveOccurred())

				Eventually(func(g Gomega) {
					err = env.Client.Get(env.Ctx, types.NamespacedName{
						Namespace: namespace,
						Name:      snapshotName,
					}, backup)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(backup.Status.BackupSnapshotStatus.Elements).To(HaveLen(2))
					g.Expect(backup.Status.Phase).To(BeEquivalentTo(apiv1.BackupPhaseCompleted))
				}, testTimeouts[testUtils.VolumeSnapshotIsReady]).Should(Succeed())
			})

			By("fetching the volume snapshots", func() {
				snapshotList := volumesnapshot.VolumeSnapshotList{}
				err := env.Client.List(env.Ctx, &snapshotList, k8client.MatchingLabels{
					utils.ClusterLabelName: clusterName,
				})
				Expect(err).ToNot(HaveOccurred())
				Expect(snapshotList.Items).To(HaveLen(len(backup.Status.BackupSnapshotStatus.Elements)))

				envVars := testUtils.EnvVarsForSnapshots{
					DataSnapshot: snapshotDataEnv,
					WalSnapshot:  snapshotWalEnv,
				}
				err = testUtils.SetSnapshotNameAsEnv(&snapshotList, backup, envVars)
				Expect(err).ToNot(HaveOccurred())
			})

			By("creating a replica cluster from the snapshot", func() {
				AssertReplicaModeCluster(
					namespace,
					clusterName,
					sourceDBName,
					replicaClusterSample,
					testTableName,
				)
			})
		})
	})
})

// assertReplicaClusterTopology asserts that the replica cluster topology is correct
// it verifies that the designated primary is streaming from the source
// and that the replicas are only streaming from the designated primary
func assertReplicaClusterTopology(namespace, clusterName string) {
	var (
		timeout        = 120
		commandTimeout = time.Second * 10

		sourceHost, primary string
		standbys            []string
	)

	cluster, err := env.GetCluster(namespace, clusterName)
	Expect(err).ToNot(HaveOccurred())
	Expect(cluster.Status.ReadyInstances).To(BeEquivalentTo(cluster.Spec.Instances))

	Expect(cluster.Spec.ExternalClusters).Should(HaveLen(1))
	sourceHost = cluster.Spec.ExternalClusters[0].ConnectionParameters["host"]
	Expect(sourceHost).ToNot(BeEmpty())

	primary = cluster.Status.CurrentPrimary
	standbys = funk.FilterString(cluster.Status.InstanceNames, func(name string) bool { return name != primary })

	getStreamingInfo := func(podName string) ([]string, error) {
		stdout, _, err := env.ExecCommandInInstancePod(
			testUtils.PodLocator{
				Namespace: namespace,
				PodName:   podName,
			},
			&commandTimeout,
			"psql", "-U", "postgres", "-tAc",
			"select string_agg(application_name, ',') from pg_stat_replication;",
		)
		if err != nil {
			return nil, err
		}
		stdout = strings.TrimSpace(stdout)
		if stdout == "" {
			return []string{}, nil
		}
		return strings.Split(stdout, ","), err
	}

	By("verifying that the standby is not streaming to any other instance", func() {
		Eventually(func(g Gomega) {
			for _, standby := range standbys {
				streamingInstances, err := getStreamingInfo(standby)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(streamingInstances).To(BeEmpty(),
					fmt.Sprintf("the standby %s should not stream to any other instance", standby),
				)
			}
		}, timeout).ShouldNot(HaveOccurred())
	})

	By("verifying that the new primary is streaming to all standbys", func() {
		Eventually(func(g Gomega) {
			streamingInstances, err := getStreamingInfo(primary)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(streamingInstances).To(
				ContainElements(standbys),
				"not all standbys are streaming from the new primary "+primary,
			)
		}, timeout).ShouldNot(HaveOccurred())
	})

	By("verifying that the new primary is streaming from the source cluster", func() {
		Eventually(func(g Gomega) {
			stdout, _, err := env.ExecCommandInInstancePod(
				testUtils.PodLocator{
					Namespace: namespace,
					PodName:   primary,
				},
				&commandTimeout,
				"psql", "-U", "postgres", "-tAc",
				"select sender_host from pg_stat_wal_receiver limit 1;",
			)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(strings.TrimSpace(stdout)).To(BeEquivalentTo(sourceHost))
		}, timeout).ShouldNot(HaveOccurred())
	})
}
