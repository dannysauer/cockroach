// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package password_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/security/password"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

func TestBCryptCostToSCRAMIterCountTable(t *testing.T) {
	defer leaktest.AfterTest(t)()

	// This test mainly checks that the conversion table properly starts
	// at bcrypt.MinCost, and that no lines are accidentally added or removed.
	require.Equal(t, bcrypt.MaxCost+1, len(password.BcryptCostToSCRAMIterCount))
	for i := 0; i < bcrypt.MinCost; i++ {
		require.Equal(t, int64(0), password.BcryptCostToSCRAMIterCount[i])
	}
	require.Equal(t, int64(4096), password.BcryptCostToSCRAMIterCount[bcrypt.MinCost])
	require.Equal(t, int64(119680), password.BcryptCostToSCRAMIterCount[10])
}

func TestBCryptToSCRAMConversion(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()
	s := cluster.MakeTestingClusterSettings()

	const cleartext = "hello"

	ourDefault := int(security.BcryptCost.Get(&s.SV))

	// Don't iterate all the way to 31: the larger values
	// incur incredibly large hashing times.
	for i := bcrypt.MinCost; i < ourDefault+3; i++ {
		t.Run(fmt.Sprintf("bcrypt=%d", i), func(t *testing.T) {
			bcryptRaw, err := bcrypt.GenerateFromPassword(password.AppendEmptySha256(cleartext), i)
			require.NoError(t, err)
			bh := password.LoadPasswordHash(ctx, bcryptRaw)
			require.Equal(t, password.HashBCrypt, bh.Method())

			// Check conversion succeeds.
			autoUpgradePasswordHashesBool := security.AutoUpgradePasswordHashes.Get(&s.SV)
			autoDowngradePasswordHashesBool := security.AutoDowngradePasswordHashes.Get(&s.SV)
			method := security.GetConfiguredPasswordHashMethod(&s.SV)
			converted, prevHash, newHashBytes, hashMethod, err := password.MaybeConvertPasswordHash(
				ctx, autoUpgradePasswordHashesBool, autoDowngradePasswordHashesBool,
				method, cleartext, bh, nil, log.Infof,
			)
			require.NoError(t, err)
			require.True(t, converted)
			require.Equal(t, "SCRAM-SHA-256", string(newHashBytes)[:13])
			require.Equal(t, password.HashSCRAMSHA256.String(), hashMethod)
			require.Equal(t, bcryptRaw, prevHash)

			newHash := password.LoadPasswordHash(ctx, newHashBytes)
			ok, creds := password.GetSCRAMStoredCredentials(newHash)
			require.Equal(t, password.HashSCRAMSHA256, newHash.Method())
			require.True(t, ok)
			require.Equal(t, password.BcryptCostToSCRAMIterCount[i], int64(creds.Iters))

			// Check that converted hash can't be converted further.
			autoUpgradePasswordHashesBool = security.AutoUpgradePasswordHashes.Get(&s.SV)
			method = security.GetConfiguredPasswordHashMethod(&s.SV)

			ec, _, _, _, err := password.MaybeConvertPasswordHash(
				ctx, autoUpgradePasswordHashesBool, autoDowngradePasswordHashesBool,
				method, cleartext, newHash, nil, log.Infof,
			)
			require.NoError(t, err)
			require.False(t, ec)
		})
	}
}

func TestSCRAMToBCryptConversion(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()
	s := cluster.MakeTestingClusterSettings()

	const cleartext = "hello"

	ourDefault := int(security.BcryptCost.Get(&s.SV))
	security.PasswordHashMethod.Override(ctx, &s.SV, int64(password.HashBCrypt))

	// Don't iterate all the way to the max cost: the larger values
	// incur incredibly large hashing times.
	for i := bcrypt.MinCost; i < ourDefault+3; i++ {
		iterCount := password.BcryptCostToSCRAMIterCount[i]
		t.Run(fmt.Sprintf("bcrypt=%d", i), func(t *testing.T) {

			scramRaw, err := password.HashPassword(ctx, int(iterCount), password.HashSCRAMSHA256, cleartext, nil /* hashSem */)
			require.NoError(t, err)
			sh := password.LoadPasswordHash(ctx, scramRaw)
			require.Equal(t, password.HashSCRAMSHA256, sh.Method())

			// Check conversion succeeds.
			autoUpgradePasswordHashesBool := security.AutoUpgradePasswordHashes.Get(&s.SV)
			autoDowngradePasswordHashesBool := security.AutoDowngradePasswordHashes.Get(&s.SV)
			method := security.GetConfiguredPasswordHashMethod(&s.SV)
			converted, prevHash, newHashBytes, hashMethod, err := password.MaybeConvertPasswordHash(
				ctx, autoUpgradePasswordHashesBool, autoDowngradePasswordHashesBool,
				method, cleartext, sh, nil, log.Infof,
			)
			require.NoError(t, err)
			require.True(t, converted)
			require.Equal(t, password.HashBCrypt.String(), hashMethod)
			require.Equal(t, scramRaw, prevHash)

			newHash := password.LoadPasswordHash(ctx, newHashBytes)
			cost, err := bcrypt.Cost(newHashBytes)
			require.Equal(t, password.HashBCrypt, newHash.Method())
			require.NoError(t, err)
			require.Equal(t, i, cost)

			// Check that converted hash can't be converted further.
			autoUpgradePasswordHashesBool = security.AutoUpgradePasswordHashes.Get(&s.SV)
			method = security.GetConfiguredPasswordHashMethod(&s.SV)

			ec, _, _, _, err := password.MaybeConvertPasswordHash(
				ctx, autoUpgradePasswordHashesBool, autoDowngradePasswordHashesBool,
				method, cleartext, newHash, nil, log.Infof,
			)
			require.NoError(t, err)
			require.False(t, ec)
		})
	}
}
