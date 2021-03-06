// Copyright (C) 2018 Storj Labs, Inc.
// See LICENSE for copying information.

package satellite

import (
	"context"
	"crypto/subtle"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/skyrings/skyring-common/tools/uuid"
	"github.com/zeebo/errs"

	"go.uber.org/zap"

	monkit "gopkg.in/spacemonkeygo/monkit.v2"

	"storj.io/storj/pkg/auth"
	"storj.io/storj/pkg/satellite/satelliteauth"
	"storj.io/storj/pkg/utils"
)

var (
	mon = monkit.Package()
)

// maxLimit specifies the limit for all paged queries
const maxLimit = 50

// Service is handling accounts related logic
type Service struct {
	Signer

	store DB
	log   *zap.Logger
}

// NewService returns new instance of Service
func NewService(log *zap.Logger, signer Signer, store DB) (*Service, error) {
	if signer == nil {
		return nil, errs.New("signer can't be nil")
	}

	if store == nil {
		return nil, errs.New("store can't be nil")
	}

	if log == nil {
		return nil, errs.New("log can't be nil")
	}

	return &Service{Signer: signer, store: store, log: log}, nil
}

// CreateUser gets password hash value and creates new User
func (s *Service) CreateUser(ctx context.Context, user CreateUser) (u *User, err error) {
	defer mon.Task()(&ctx)(&err)
	if err := user.IsValid(); err != nil {
		return nil, err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(user.Password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	//passwordHash := sha256.Sum256()
	return s.store.Users().Insert(ctx, &User{
		Email:        user.Email,
		FirstName:    user.FirstName,
		LastName:     user.LastName,
		PasswordHash: hash,
	})
}

// Token authenticates User by credentials and returns auth token
func (s *Service) Token(ctx context.Context, email, password string) (token string, err error) {
	defer mon.Task()(&ctx)(&err)
	user, err := s.store.Users().GetByEmail(ctx, email)
	if err != nil {
		return "", err
	}

	err = bcrypt.CompareHashAndPassword(user.PasswordHash, []byte(password))
	if err != nil {
		return "", ErrUnauthorized.New("password is incorrect: %s", err.Error())
	}

	// TODO: move expiration time to constants
	claims := satelliteauth.Claims{
		ID:         user.ID,
		Expiration: time.Now().Add(time.Minute * 15),
	}

	token, err = s.createToken(&claims)
	if err != nil {
		return "", err
	}

	return token, nil
}

// GetUser returns User by id
func (s *Service) GetUser(ctx context.Context, id uuid.UUID) (u *User, err error) {
	defer mon.Task()(&ctx)(&err)
	_, err = GetAuth(ctx)
	if err != nil {
		return nil, err
	}

	return s.store.Users().Get(ctx, id)
}

// UpdateUser updates User with given id
func (s *Service) UpdateUser(ctx context.Context, id uuid.UUID, info UserInfo) (err error) {
	defer mon.Task()(&ctx)(&err)
	_, err = GetAuth(ctx)
	if err != nil {
		return err
	}

	if err = info.IsValid(); err != nil {
		return err
	}

	return s.store.Users().Update(ctx, &User{
		ID:           id,
		FirstName:    info.FirstName,
		LastName:     info.LastName,
		Email:        info.Email,
		PasswordHash: nil,
	})
}

// ChangeUserPassword updates password for a given user
func (s *Service) ChangeUserPassword(ctx context.Context, id uuid.UUID, pass, newPass string) (err error) {
	defer mon.Task()(&ctx)(&err)
	_, err = GetAuth(ctx)
	if err != nil {
		return err
	}

	user, err := s.store.Users().Get(ctx, id)
	if err != nil {
		return err
	}

	err = bcrypt.CompareHashAndPassword(user.PasswordHash, []byte(pass))
	if err != nil {
		return ErrUnauthorized.New("origin password is incorrect: %s", err.Error())
	}

	if err := validatePassword(newPass); err != nil {
		return err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(newPass), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	user.PasswordHash = hash
	return s.store.Users().Update(ctx, user)
}

// DeleteUser deletes User by id
func (s *Service) DeleteUser(ctx context.Context, id uuid.UUID, password string) (err error) {
	defer mon.Task()(&ctx)(&err)
	auth, err := GetAuth(ctx)
	if err != nil {
		return err
	}

	if !uuid.Equal(auth.User.ID, id) {
		return ErrUnauthorized.New("user has no rights")
	}

	err = bcrypt.CompareHashAndPassword(auth.User.PasswordHash, []byte(password))
	if err != nil {
		return ErrUnauthorized.New("origin password is incorrect")
	}

	return s.store.Users().Delete(ctx, id)
}

// GetProject is a method for querying project by id
func (s *Service) GetProject(ctx context.Context, projectID uuid.UUID) (p *Project, err error) {
	defer mon.Task()(&ctx)(&err)
	_, err = GetAuth(ctx)
	if err != nil {
		return nil, err
	}

	return s.store.Projects().Get(ctx, projectID)
}

// GetUsersProjects is a method for querying all projects
func (s *Service) GetUsersProjects(ctx context.Context) (ps []Project, err error) {
	defer mon.Task()(&ctx)(&err)
	auth, err := GetAuth(ctx)
	if err != nil {
		return nil, err
	}

	return s.store.Projects().GetByUserID(ctx, auth.User.ID)
}

// CreateProject is a method for creating new project
func (s *Service) CreateProject(ctx context.Context, projectInfo ProjectInfo) (p *Project, err error) {
	defer mon.Task()(&ctx)(&err)
	auth, err := GetAuth(ctx)
	if err != nil {
		return nil, err
	}

	if !projectInfo.IsTermsAccepted {
		return nil, errs.New("terms of use should be accepted!")
	}

	project := &Project{
		Description:   projectInfo.Description,
		Name:          projectInfo.Name,
		TermsAccepted: 1, //TODO: get lat version of Term of Use
	}

	transaction, err := s.store.BeginTx(ctx)
	if err != nil {
		return nil, err
	}

	prj, err := transaction.Projects().Insert(ctx, project)
	if err != nil {
		return nil, utils.CombineErrors(err, transaction.Rollback())
	}

	_, err = transaction.ProjectMembers().Insert(ctx, auth.User.ID, prj.ID)
	if err != nil {
		return nil, utils.CombineErrors(err, transaction.Rollback())
	}

	return prj, transaction.Commit()
}

// DeleteProject is a method for deleting project by id
func (s *Service) DeleteProject(ctx context.Context, projectID uuid.UUID) (err error) {
	defer mon.Task()(&ctx)(&err)
	_, err = GetAuth(ctx)
	if err != nil {
		return err
	}

	// TODO: before deletion we should check if user is a project member
	return s.store.Projects().Delete(ctx, projectID)
}

// UpdateProject is a method for updating project description by id
func (s *Service) UpdateProject(ctx context.Context, projectID uuid.UUID, description string) (p *Project, err error) {
	defer mon.Task()(&ctx)(&err)
	_, err = GetAuth(ctx)
	if err != nil {
		return nil, err
	}

	project, err := s.store.Projects().Get(ctx, projectID)
	if err != nil {
		return nil, errs.New("Project doesn't exist!")
	}

	project.Description = description

	err = s.store.Projects().Update(ctx, project)
	if err != nil {
		return nil, err
	}

	return project, nil
}

// AddProjectMember adds User as member of given Project
func (s *Service) AddProjectMember(ctx context.Context, projectID, userID uuid.UUID) (err error) {
	defer mon.Task()(&ctx)(&err)
	_, err = GetAuth(ctx)
	if err != nil {
		return err
	}

	_, err = s.store.ProjectMembers().Insert(ctx, userID, projectID)
	return err
}

// DeleteProjectMember removes user membership for given project
func (s *Service) DeleteProjectMember(ctx context.Context, projectID, userID uuid.UUID) (err error) {
	defer mon.Task()(&ctx)(&err)
	_, err = GetAuth(ctx)
	if err != nil {
		return err
	}

	return s.store.ProjectMembers().Delete(ctx, userID, projectID)
}

// GetProjectMembers returns ProjectMembers for given Project
func (s *Service) GetProjectMembers(ctx context.Context, projectID uuid.UUID, limit int, offset int64) (pm []ProjectMember, err error) {
	defer mon.Task()(&ctx)(&err)
	_, err = GetAuth(ctx)
	if err != nil {
		return nil, err
	}

	if limit < 0 || offset < 0 {
		return nil, errs.New("invalid pagination argument")
	}

	if limit > maxLimit {
		limit = maxLimit
	}

	return s.store.ProjectMembers().GetByProjectID(ctx, projectID, limit, offset)
}

// Authorize validates token from context and returns authorized Authorization
func (s *Service) Authorize(ctx context.Context) (a Authorization, err error) {
	defer mon.Task()(&ctx)(&err)
	tokenS, ok := auth.GetAPIKey(ctx)
	if !ok {
		return Authorization{}, ErrUnauthorized.New("no api key was provided")
	}

	token, err := satelliteauth.FromBase64URLString(string(tokenS))
	if err != nil {
		return Authorization{}, ErrUnauthorized.Wrap(err)
	}

	claims, err := s.authenticate(token)
	if err != nil {
		return Authorization{}, ErrUnauthorized.Wrap(err)
	}

	user, err := s.authorize(ctx, claims)
	if err != nil {
		return Authorization{}, ErrUnauthorized.Wrap(err)
	}

	return Authorization{
		User:   *user,
		Claims: *claims,
	}, nil
}

// createToken creates string representation
func (s *Service) createToken(claims *satelliteauth.Claims) (string, error) {
	json, err := claims.JSON()
	if err != nil {
		return "", err
	}

	token := satelliteauth.Token{Payload: json}
	err = signToken(&token, s.Signer)
	if err != nil {
		return "", err
	}

	return token.String(), nil
}

// authenticate validates token signature and returns authenticated *satelliteauth.Authorization
func (s *Service) authenticate(token satelliteauth.Token) (*satelliteauth.Claims, error) {
	signature := token.Signature

	err := signToken(&token, s.Signer)
	if err != nil {
		return nil, err
	}

	if subtle.ConstantTimeCompare(signature, token.Signature) != 1 {
		return nil, errs.New("incorrect signature")
	}

	claims, err := satelliteauth.FromJSON(token.Payload)
	if err != nil {
		return nil, err
	}

	return claims, nil
}

// authorize checks claims and returns authorized User
func (s *Service) authorize(ctx context.Context, claims *satelliteauth.Claims) (*User, error) {
	if !claims.Expiration.IsZero() && claims.Expiration.Before(time.Now()) {
		return nil, errs.New("token is outdated")
	}

	user, err := s.store.Users().Get(ctx, claims.ID)
	if err != nil {
		return nil, errs.New("authorization failed. no user with id: %s", claims.ID.String())
	}

	return user, nil
}
