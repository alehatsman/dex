import { AuthService } from './auth';
import { Logger } from './logger';
import { Cache } from './cache';

// Same-file class used to exercise a typed field whose type is local.
class Session {
  end(): void {}
}

export class UserService {
  // field initialized by construction (imported type)
  private cache = new Cache();
  // typed field declaration (same-file type)
  protected session: Session = new Session();

  constructor(
    // constructor parameter-properties (imported types)
    private readonly auth: AuthService,
    public log: Logger,
  ) {}

  doLogin(): void {
    this.auth.login(); // → AuthService.login  (param-property, imported)
    this.log.info("x"); // → Logger.info        (param-property, imported)
    this.cache.get(); // → Cache.get          (field = new T(), imported)
    this.session.end(); // → Session.end        (typed field, same-file)
  }
}
